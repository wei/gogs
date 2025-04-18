// Copyright 2014 The Gogs Authors. All rights reserved.
// Use of this source code is governed by a MIT-style
// license that can be found in the LICENSE file.

package repo

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/gogs/git-module"
	"github.com/unknwon/com"
	log "unknwon.dev/clog/v2"

	"gogs.io/gogs/internal/conf"
	"gogs.io/gogs/internal/context"
	"gogs.io/gogs/internal/database"
	"gogs.io/gogs/internal/database/errors"
	"gogs.io/gogs/internal/email"
	"gogs.io/gogs/internal/form"
	"gogs.io/gogs/internal/osutil"
	"gogs.io/gogs/internal/tool"
	"gogs.io/gogs/internal/userutil"
)

const (
	tmplRepoSettingsOptions         = "repo/settings/options"
	tmplRepoSettingsAvatar          = "repo/settings/avatar"
	tmplRepoSettingsCollaboration   = "repo/settings/collaboration"
	tmplRepoSettingsBranches        = "repo/settings/branches"
	tmplRepoSettingsProtectedBranch = "repo/settings/protected_branch"
	tmplRepoSettingsGithooks        = "repo/settings/githooks"
	tmplRepoSettingsGithookEdit     = "repo/settings/githook_edit"
	tmplRepoSettingsDeployKeys      = "repo/settings/deploy_keys"
)

func Settings(c *context.Context) {
	c.Title("repo.settings")
	c.PageIs("SettingsOptions")
	c.RequireAutosize()
	c.Success(tmplRepoSettingsOptions)
}

func SettingsPost(c *context.Context, f form.RepoSetting) {
	c.Title("repo.settings")
	c.PageIs("SettingsOptions")
	c.RequireAutosize()

	repo := c.Repo.Repository

	switch c.Query("action") {
	case "update":
		if c.HasError() {
			c.Success(tmplRepoSettingsOptions)
			return
		}

		isNameChanged := false
		oldRepoName := repo.Name
		newRepoName := f.RepoName
		// Check if repository name has been changed.
		if repo.LowerName != strings.ToLower(newRepoName) {
			isNameChanged = true
			if err := database.ChangeRepositoryName(c.Repo.Owner, repo.Name, newRepoName); err != nil {
				c.FormErr("RepoName")
				switch {
				case database.IsErrRepoAlreadyExist(err):
					c.RenderWithErr(c.Tr("form.repo_name_been_taken"), tmplRepoSettingsOptions, &f)
				case database.IsErrNameNotAllowed(err):
					c.RenderWithErr(c.Tr("repo.form.name_not_allowed", err.(database.ErrNameNotAllowed).Value()), tmplRepoSettingsOptions, &f)
				default:
					c.Error(err, "change repository name")
				}
				return
			}

			log.Trace("Repository name changed: %s/%s -> %s", c.Repo.Owner.Name, repo.Name, newRepoName)
		}
		// In case it's just a case change.
		repo.Name = newRepoName
		repo.LowerName = strings.ToLower(newRepoName)

		repo.Description = f.Description
		repo.Website = f.Website

		// Visibility of forked repository is forced sync with base repository.
		if repo.IsFork {
			f.Private = repo.BaseRepo.IsPrivate
			f.Unlisted = repo.BaseRepo.IsUnlisted
		}

		visibilityChanged := repo.IsPrivate != f.Private || repo.IsUnlisted != f.Unlisted
		repo.IsPrivate = f.Private
		repo.IsUnlisted = f.Unlisted
		if err := database.UpdateRepository(repo, visibilityChanged); err != nil {
			c.Error(err, "update repository")
			return
		}
		log.Trace("Repository basic settings updated: %s/%s", c.Repo.Owner.Name, repo.Name)

		if isNameChanged {
			if err := database.Handle.Actions().RenameRepo(c.Req.Context(), c.User, repo.MustOwner(), oldRepoName, repo); err != nil {
				log.Error("create rename repository action: %v", err)
			}
		}

		c.Flash.Success(c.Tr("repo.settings.update_settings_success"))
		c.Redirect(repo.Link() + "/settings")

	case "mirror":
		if !repo.IsMirror {
			c.NotFound()
			return
		}

		if f.Interval > 0 {
			c.Repo.Mirror.EnablePrune = f.EnablePrune
			c.Repo.Mirror.Interval = f.Interval
			c.Repo.Mirror.NextSync = time.Now().Add(time.Duration(f.Interval) * time.Hour)
			if err := database.UpdateMirror(c.Repo.Mirror); err != nil {
				c.Error(err, "update mirror")
				return
			}
		}
		if err := c.Repo.Mirror.SaveAddress(f.MirrorAddress); err != nil {
			c.Error(err, "save address")
			return
		}

		c.Flash.Success(c.Tr("repo.settings.update_settings_success"))
		c.Redirect(repo.Link() + "/settings")

	case "mirror-sync":
		if !repo.IsMirror {
			c.NotFound()
			return
		}

		go database.MirrorQueue.Add(repo.ID)
		c.Flash.Info(c.Tr("repo.settings.mirror_sync_in_progress"))
		c.Redirect(repo.Link() + "/settings")

	case "advanced":
		repo.EnableWiki = f.EnableWiki
		repo.AllowPublicWiki = f.AllowPublicWiki
		repo.EnableExternalWiki = f.EnableExternalWiki
		repo.ExternalWikiURL = f.ExternalWikiURL
		repo.EnableIssues = f.EnableIssues
		repo.AllowPublicIssues = f.AllowPublicIssues
		repo.EnableExternalTracker = f.EnableExternalTracker
		repo.ExternalTrackerURL = f.ExternalTrackerURL
		repo.ExternalTrackerFormat = f.TrackerURLFormat
		repo.ExternalTrackerStyle = f.TrackerIssueStyle
		repo.EnablePulls = f.EnablePulls
		repo.PullsIgnoreWhitespace = f.PullsIgnoreWhitespace
		repo.PullsAllowRebase = f.PullsAllowRebase

		if !repo.EnableWiki || repo.EnableExternalWiki {
			repo.AllowPublicWiki = false
		}
		if !repo.EnableIssues || repo.EnableExternalTracker {
			repo.AllowPublicIssues = false
		}

		if err := database.UpdateRepository(repo, false); err != nil {
			c.Error(err, "update repository")
			return
		}
		log.Trace("Repository advanced settings updated: %s/%s", c.Repo.Owner.Name, repo.Name)

		c.Flash.Success(c.Tr("repo.settings.update_settings_success"))
		c.Redirect(c.Repo.RepoLink + "/settings")

	case "convert":
		if !c.Repo.IsOwner() {
			c.NotFound()
			return
		}
		if repo.Name != f.RepoName {
			c.RenderWithErr(c.Tr("form.enterred_invalid_repo_name"), tmplRepoSettingsOptions, nil)
			return
		}

		if c.Repo.Owner.IsOrganization() {
			if !c.Repo.Owner.IsOwnedBy(c.User.ID) {
				c.NotFound()
				return
			}
		}

		if !repo.IsMirror {
			c.NotFound()
			return
		}
		repo.IsMirror = false

		if _, err := database.CleanUpMigrateInfo(repo); err != nil {
			c.Error(err, "clean up migrate info")
			return
		} else if err = database.DeleteMirrorByRepoID(c.Repo.Repository.ID); err != nil {
			c.Error(err, "delete mirror by repository ID")
			return
		}
		log.Trace("Repository converted from mirror to regular: %s/%s", c.Repo.Owner.Name, repo.Name)
		c.Flash.Success(c.Tr("repo.settings.convert_succeed"))
		c.Redirect(conf.Server.Subpath + "/" + c.Repo.Owner.Name + "/" + repo.Name)

	case "transfer":
		if !c.Repo.IsOwner() {
			c.NotFound()
			return
		}
		if repo.Name != f.RepoName {
			c.RenderWithErr(c.Tr("form.enterred_invalid_repo_name"), tmplRepoSettingsOptions, nil)
			return
		}

		if c.Repo.Owner.IsOrganization() && !c.User.IsAdmin {
			if !c.Repo.Owner.IsOwnedBy(c.User.ID) {
				c.NotFound()
				return
			}
		}

		newOwner := c.Query("new_owner_name")
		if !database.Handle.Users().IsUsernameUsed(c.Req.Context(), newOwner, c.Repo.Owner.ID) {
			c.RenderWithErr(c.Tr("form.enterred_invalid_owner_name"), tmplRepoSettingsOptions, nil)
			return
		}

		if err := database.TransferOwnership(c.User, newOwner, repo); err != nil {
			if database.IsErrRepoAlreadyExist(err) {
				c.RenderWithErr(c.Tr("repo.settings.new_owner_has_same_repo"), tmplRepoSettingsOptions, nil)
			} else {
				c.Error(err, "transfer ownership")
			}
			return
		}
		log.Trace("Repository transferred: %s/%s -> %s", c.Repo.Owner.Name, repo.Name, newOwner)
		c.Flash.Success(c.Tr("repo.settings.transfer_succeed"))
		c.Redirect(conf.Server.Subpath + "/" + newOwner + "/" + repo.Name)

	case "delete":
		if !c.Repo.IsOwner() {
			c.NotFound()
			return
		}
		if repo.Name != f.RepoName {
			c.RenderWithErr(c.Tr("form.enterred_invalid_repo_name"), tmplRepoSettingsOptions, nil)
			return
		}

		if c.Repo.Owner.IsOrganization() && !c.User.IsAdmin {
			if !c.Repo.Owner.IsOwnedBy(c.User.ID) {
				c.NotFound()
				return
			}
		}

		if err := database.DeleteRepository(c.Repo.Owner.ID, repo.ID); err != nil {
			c.Error(err, "delete repository")
			return
		}
		log.Trace("Repository deleted: %s/%s", c.Repo.Owner.Name, repo.Name)

		c.Flash.Success(c.Tr("repo.settings.deletion_success"))
		c.Redirect(userutil.DashboardURLPath(c.Repo.Owner.Name, c.Repo.Owner.IsOrganization()))

	case "delete-wiki":
		if !c.Repo.IsOwner() {
			c.NotFound()
			return
		}
		if repo.Name != f.RepoName {
			c.RenderWithErr(c.Tr("form.enterred_invalid_repo_name"), tmplRepoSettingsOptions, nil)
			return
		}

		if c.Repo.Owner.IsOrganization() && !c.User.IsAdmin {
			if !c.Repo.Owner.IsOwnedBy(c.User.ID) {
				c.NotFound()
				return
			}
		}

		repo.DeleteWiki()
		log.Trace("Repository wiki deleted: %s/%s", c.Repo.Owner.Name, repo.Name)

		repo.EnableWiki = false
		if err := database.UpdateRepository(repo, false); err != nil {
			c.Error(err, "update repository")
			return
		}

		c.Flash.Success(c.Tr("repo.settings.wiki_deletion_success"))
		c.Redirect(c.Repo.RepoLink + "/settings")

	default:
		c.NotFound()
	}
}

func SettingsAvatar(c *context.Context) {
	c.Title("settings.avatar")
	c.PageIs("SettingsAvatar")
	c.Success(tmplRepoSettingsAvatar)
}

func SettingsAvatarPost(c *context.Context, f form.Avatar) {
	f.Source = form.AvatarLocal
	if err := UpdateAvatarSetting(c, f, c.Repo.Repository); err != nil {
		c.Flash.Error(err.Error())
	} else {
		c.Flash.Success(c.Tr("settings.update_avatar_success"))
	}
	c.RedirectSubpath(c.Repo.RepoLink + "/settings")
}

func SettingsDeleteAvatar(c *context.Context) {
	if err := c.Repo.Repository.DeleteAvatar(); err != nil {
		c.Flash.Error(fmt.Sprintf("Failed to delete avatar: %v", err))
	}
	c.RedirectSubpath(c.Repo.RepoLink + "/settings")
}

// FIXME: limit upload size
func UpdateAvatarSetting(c *context.Context, f form.Avatar, ctxRepo *database.Repository) error {
	ctxRepo.UseCustomAvatar = true
	if f.Avatar != nil {
		r, err := f.Avatar.Open()
		if err != nil {
			return fmt.Errorf("open avatar reader: %v", err)
		}
		defer r.Close()

		data, err := io.ReadAll(r)
		if err != nil {
			return fmt.Errorf("read avatar content: %v", err)
		}
		if !tool.IsImageFile(data) {
			return errors.New(c.Tr("settings.uploaded_avatar_not_a_image"))
		}
		if err = ctxRepo.UploadAvatar(data); err != nil {
			return fmt.Errorf("upload avatar: %v", err)
		}
	} else {
		// No avatar is uploaded and reset setting back.
		if !com.IsFile(ctxRepo.CustomAvatarPath()) {
			ctxRepo.UseCustomAvatar = false
		}
	}

	if err := database.UpdateRepository(ctxRepo, false); err != nil {
		return fmt.Errorf("update repository: %v", err)
	}

	return nil
}

func SettingsCollaboration(c *context.Context) {
	c.Data["Title"] = c.Tr("repo.settings")
	c.Data["PageIsSettingsCollaboration"] = true

	users, err := c.Repo.Repository.GetCollaborators()
	if err != nil {
		c.Error(err, "get collaborators")
		return
	}
	c.Data["Collaborators"] = users

	c.Success(tmplRepoSettingsCollaboration)
}

func SettingsCollaborationPost(c *context.Context) {
	name := strings.ToLower(c.Query("collaborator"))
	if name == "" || c.Repo.Owner.LowerName == name {
		c.Redirect(conf.Server.Subpath + c.Req.URL.Path)
		return
	}

	u, err := database.Handle.Users().GetByUsername(c.Req.Context(), name)
	if err != nil {
		if database.IsErrUserNotExist(err) {
			c.Flash.Error(c.Tr("form.user_not_exist"))
			c.Redirect(conf.Server.Subpath + c.Req.URL.Path)
		} else {
			c.Error(err, "get user by name")
		}
		return
	}

	// Organization is not allowed to be added as a collaborator
	if u.IsOrganization() {
		c.Flash.Error(c.Tr("repo.settings.org_not_allowed_to_be_collaborator"))
		c.Redirect(conf.Server.Subpath + c.Req.URL.Path)
		return
	}

	if err = c.Repo.Repository.AddCollaborator(u); err != nil {
		c.Error(err, "add collaborator")
		return
	}

	if conf.User.EnableEmailNotification {
		email.SendCollaboratorMail(database.NewMailerUser(u), database.NewMailerUser(c.User), database.NewMailerRepo(c.Repo.Repository))
	}

	c.Flash.Success(c.Tr("repo.settings.add_collaborator_success"))
	c.Redirect(conf.Server.Subpath + c.Req.URL.Path)
}

func ChangeCollaborationAccessMode(c *context.Context) {
	if err := c.Repo.Repository.ChangeCollaborationAccessMode(
		c.QueryInt64("uid"),
		database.AccessMode(c.QueryInt("mode"))); err != nil {
		log.Error("ChangeCollaborationAccessMode: %v", err)
		return
	}

	c.Status(204)
}

func DeleteCollaboration(c *context.Context) {
	if err := c.Repo.Repository.DeleteCollaboration(c.QueryInt64("id")); err != nil {
		c.Flash.Error("DeleteCollaboration: " + err.Error())
	} else {
		c.Flash.Success(c.Tr("repo.settings.remove_collaborator_success"))
	}

	c.JSONSuccess(map[string]any{
		"redirect": c.Repo.RepoLink + "/settings/collaboration",
	})
}

func SettingsBranches(c *context.Context) {
	c.Data["Title"] = c.Tr("repo.settings.branches")
	c.Data["PageIsSettingsBranches"] = true

	if c.Repo.Repository.IsBare {
		c.Flash.Info(c.Tr("repo.settings.branches_bare"), true)
		c.Success(tmplRepoSettingsBranches)
		return
	}

	protectBranches, err := database.GetProtectBranchesByRepoID(c.Repo.Repository.ID)
	if err != nil {
		c.Error(err, "get protect branch by repository ID")
		return
	}

	// Filter out deleted branches
	branches := make([]string, 0, len(protectBranches))
	for i := range protectBranches {
		if c.Repo.GitRepo.HasBranch(protectBranches[i].Name) {
			branches = append(branches, protectBranches[i].Name)
		}
	}
	c.Data["ProtectBranches"] = branches

	c.Success(tmplRepoSettingsBranches)
}

func UpdateDefaultBranch(c *context.Context) {
	branch := c.Query("branch")
	if c.Repo.GitRepo.HasBranch(branch) &&
		c.Repo.Repository.DefaultBranch != branch {
		c.Repo.Repository.DefaultBranch = branch
		if _, err := c.Repo.GitRepo.SymbolicRef(git.SymbolicRefOptions{
			Ref: git.RefsHeads + branch,
		}); err != nil {
			c.Flash.Warning(c.Tr("repo.settings.update_default_branch_unsupported"))
			c.Redirect(c.Repo.RepoLink + "/settings/branches")
			return
		}
	}

	if err := database.UpdateRepository(c.Repo.Repository, false); err != nil {
		c.Error(err, "update repository")
		return
	}

	c.Flash.Success(c.Tr("repo.settings.update_default_branch_success"))
	c.Redirect(c.Repo.RepoLink + "/settings/branches")
}

func SettingsProtectedBranch(c *context.Context) {
	branch := c.Params("*")
	if !c.Repo.GitRepo.HasBranch(branch) {
		c.NotFound()
		return
	}

	c.Data["Title"] = c.Tr("repo.settings.protected_branches") + " - " + branch
	c.Data["PageIsSettingsBranches"] = true

	protectBranch, err := database.GetProtectBranchOfRepoByName(c.Repo.Repository.ID, branch)
	if err != nil {
		if !database.IsErrBranchNotExist(err) {
			c.Error(err, "get protect branch of repository by name")
			return
		}

		// No options found, create defaults.
		protectBranch = &database.ProtectBranch{
			Name: branch,
		}
	}

	if c.Repo.Owner.IsOrganization() {
		users, err := c.Repo.Repository.GetWriters()
		if err != nil {
			c.Error(err, "get writers")
			return
		}
		c.Data["Users"] = users
		c.Data["whitelist_users"] = protectBranch.WhitelistUserIDs

		teams, err := c.Repo.Owner.TeamsHaveAccessToRepo(c.Repo.Repository.ID, database.AccessModeWrite)
		if err != nil {
			c.Error(err, "get teams have access to the repository")
			return
		}
		c.Data["Teams"] = teams
		c.Data["whitelist_teams"] = protectBranch.WhitelistTeamIDs
	}

	c.Data["Branch"] = protectBranch
	c.Success(tmplRepoSettingsProtectedBranch)
}

func SettingsProtectedBranchPost(c *context.Context, f form.ProtectBranch) {
	branch := c.Params("*")
	if !c.Repo.GitRepo.HasBranch(branch) {
		c.NotFound()
		return
	}

	protectBranch, err := database.GetProtectBranchOfRepoByName(c.Repo.Repository.ID, branch)
	if err != nil {
		if !database.IsErrBranchNotExist(err) {
			c.Error(err, "get protect branch of repository by name")
			return
		}

		// No options found, create defaults.
		protectBranch = &database.ProtectBranch{
			RepoID: c.Repo.Repository.ID,
			Name:   branch,
		}
	}

	protectBranch.Protected = f.Protected
	protectBranch.RequirePullRequest = f.RequirePullRequest
	protectBranch.EnableWhitelist = f.EnableWhitelist
	if c.Repo.Owner.IsOrganization() {
		err = database.UpdateOrgProtectBranch(c.Repo.Repository, protectBranch, f.WhitelistUsers, f.WhitelistTeams)
	} else {
		err = database.UpdateProtectBranch(protectBranch)
	}
	if err != nil {
		c.Error(err, "update protect branch")
		return
	}

	c.Flash.Success(c.Tr("repo.settings.update_protect_branch_success"))
	c.Redirect(fmt.Sprintf("%s/settings/branches/%s", c.Repo.RepoLink, branch))
}

func SettingsGitHooks(c *context.Context) {
	c.Data["Title"] = c.Tr("repo.settings.githooks")
	c.Data["PageIsSettingsGitHooks"] = true

	hooks, err := c.Repo.GitRepo.Hooks("custom_hooks")
	if err != nil {
		c.Error(err, "get hooks")
		return
	}
	c.Data["Hooks"] = hooks

	c.Success(tmplRepoSettingsGithooks)
}

func SettingsGitHooksEdit(c *context.Context) {
	c.Data["Title"] = c.Tr("repo.settings.githooks")
	c.Data["PageIsSettingsGitHooks"] = true
	c.Data["RequireSimpleMDE"] = true

	name := c.Params(":name")
	hook, err := c.Repo.GitRepo.Hook("custom_hooks", git.HookName(name))
	if err != nil {
		c.NotFoundOrError(osutil.NewError(err), "get hook")
		return
	}
	c.Data["Hook"] = hook
	c.Success(tmplRepoSettingsGithookEdit)
}

func SettingsGitHooksEditPost(c *context.Context) {
	name := c.Params(":name")
	hook, err := c.Repo.GitRepo.Hook("custom_hooks", git.HookName(name))
	if err != nil {
		c.NotFoundOrError(osutil.NewError(err), "get hook")
		return
	}
	if err = hook.Update(c.Query("content")); err != nil {
		c.Error(err, "update hook")
		return
	}
	c.Redirect(c.Data["Link"].(string))
}

func SettingsDeployKeys(c *context.Context) {
	c.Data["Title"] = c.Tr("repo.settings.deploy_keys")
	c.Data["PageIsSettingsKeys"] = true

	keys, err := database.ListDeployKeys(c.Repo.Repository.ID)
	if err != nil {
		c.Error(err, "list deploy keys")
		return
	}
	c.Data["Deploykeys"] = keys

	c.Success(tmplRepoSettingsDeployKeys)
}

func SettingsDeployKeysPost(c *context.Context, f form.AddSSHKey) {
	c.Data["Title"] = c.Tr("repo.settings.deploy_keys")
	c.Data["PageIsSettingsKeys"] = true

	keys, err := database.ListDeployKeys(c.Repo.Repository.ID)
	if err != nil {
		c.Error(err, "list deploy keys")
		return
	}
	c.Data["Deploykeys"] = keys

	if c.HasError() {
		c.Success(tmplRepoSettingsDeployKeys)
		return
	}

	content, err := database.CheckPublicKeyString(f.Content)
	if err != nil {
		if database.IsErrKeyUnableVerify(err) {
			c.Flash.Info(c.Tr("form.unable_verify_ssh_key"))
		} else {
			c.Data["HasError"] = true
			c.Data["Err_Content"] = true
			c.Flash.Error(c.Tr("form.invalid_ssh_key", err.Error()))
			c.Redirect(c.Repo.RepoLink + "/settings/keys")
			return
		}
	}

	key, err := database.AddDeployKey(c.Repo.Repository.ID, f.Title, content)
	if err != nil {
		c.Data["HasError"] = true
		switch {
		case database.IsErrKeyAlreadyExist(err):
			c.Data["Err_Content"] = true
			c.RenderWithErr(c.Tr("repo.settings.key_been_used"), tmplRepoSettingsDeployKeys, &f)
		case database.IsErrKeyNameAlreadyUsed(err):
			c.Data["Err_Title"] = true
			c.RenderWithErr(c.Tr("repo.settings.key_name_used"), tmplRepoSettingsDeployKeys, &f)
		default:
			c.Error(err, "add deploy key")
		}
		return
	}

	log.Trace("Deploy key added: %d", c.Repo.Repository.ID)
	c.Flash.Success(c.Tr("repo.settings.add_key_success", key.Name))
	c.Redirect(c.Repo.RepoLink + "/settings/keys")
}

func DeleteDeployKey(c *context.Context) {
	if err := database.DeleteDeployKey(c.User, c.QueryInt64("id")); err != nil {
		c.Flash.Error("DeleteDeployKey: " + err.Error())
	} else {
		c.Flash.Success(c.Tr("repo.settings.deploy_key_deletion_success"))
	}

	c.JSONSuccess(map[string]any{
		"redirect": c.Repo.RepoLink + "/settings/keys",
	})
}
