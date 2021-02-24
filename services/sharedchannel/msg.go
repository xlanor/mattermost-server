// Copyright (c) 2015-present Mattermost, Inc. All Rights Reserved.
// See LICENSE.txt for license information.

package sharedchannel

import (
	"context"
	"encoding/json"

	"github.com/mattermost/mattermost-server/v5/mlog"
	"github.com/mattermost/mattermost-server/v5/model"
)

// syncMsg represents a change in content (post add/edit/delete, reaction add/remove, users).
// It is sent to remote clusters as the payload of a `RemoteClusterMsg`.
type syncMsg struct {
	ChannelId   string            `json:"channel_id"`
	PostId      string            `json:"post_id"`
	Post        *model.Post       `json:"post"`
	Users       []*model.User     `json:"users"`
	Reactions   []*model.Reaction `json:"reactions"`
	Attachments []*model.FileInfo `json:"-"`
}

func (sm syncMsg) ToJSON() ([]byte, error) {
	b, err := json.Marshal(sm)
	if err != nil {
		return nil, err
	}
	return b, nil
}

func (sm syncMsg) String() string {
	json, err := sm.ToJSON()
	if err != nil {
		return ""
	}
	return string(json)
}

type userCache map[string]struct{}

func (u userCache) Has(id string) bool {
	_, ok := u[id]
	return ok
}

func (u userCache) Add(id string) {
	u[id] = struct{}{}
}

// postsToSyncMessages takes a slice of posts and converts to a `RemoteClusterMsg` which can be
// sent to a remote cluster.
func (scs *Service) postsToSyncMessages(posts []*model.Post, rc *model.RemoteCluster, nextSyncAt int64) ([]syncMsg, error) {
	syncMessages := make([]syncMsg, 0, len(posts))

	uCache := make(userCache)

	for _, p := range posts {
		if p.IsSystemMessage() { // don't sync system messages
			continue
		}

		// any reactions originating from the remote cluster are filtered out
		reactions, err := scs.server.GetStore().Reaction().GetForPostSince(p.Id, nextSyncAt, rc.RemoteId, true)
		if err != nil {
			return nil, err
		}

		postSync := p

		// Don't resend an existing post where only the reactions changed.
		// Posts we must send:
		//   - new posts (EditAt == 0)
		//   - edited posts (EditAt >= nextSyncAt)
		//   - deleted posts (DeleteAt > 0)
		if p.EditAt > 0 && p.EditAt < nextSyncAt && p.DeleteAt == 0 {
			postSync = nil
		}

		// don't sync a post back to the remote it came from.
		if p.RemoteId != nil && *p.RemoteId == rc.RemoteId {
			postSync = nil
		}

		var attachments []*model.FileInfo
		if postSync != nil {
			// parse out all permalinks in the message.
			postSync.Message = scs.processPermalinkToRemote(postSync)

			// get any file attachments
			attachments, err = scs.postToAttachments(postSync, rc, nextSyncAt)
			if err != nil {
				scs.server.GetLogger().Log(mlog.LvlSharedChannelServiceError, "Could not fetch attachments for post",
					mlog.String("post_id", postSync.Id),
					mlog.Err(err),
				)
			}
		}

		// any users originating from the remote cluster are filtered out
		users := scs.usersForPost(postSync, reactions, rc, uCache)

		// if everything was filtered out then don't send an empty message.
		if postSync == nil && len(reactions) == 0 && len(users) == 0 {
			continue
		}

		sm := syncMsg{
			ChannelId:   p.ChannelId,
			PostId:      p.Id,
			Post:        postSync,
			Users:       users,
			Reactions:   reactions,
			Attachments: attachments,
		}
		syncMessages = append(syncMessages, sm)
	}
	return syncMessages, nil
}

// usersForPost provides a list of Users associated with the post that need to be synchronized.
// The user cache ensures the same user is not synchronized redundantly if they appear in multiple
// posts for this sync batch.
func (scs *Service) usersForPost(post *model.Post, reactions []*model.Reaction, rc *model.RemoteCluster, uCache userCache) []*model.User {
	userIds := make([]string, 0)

	if post != nil && !uCache.Has(post.UserId) {
		userIds = append(userIds, post.UserId)
		uCache.Add(post.UserId)
	}

	for _, r := range reactions {
		if !uCache.Has(r.UserId) {
			userIds = append(userIds, r.UserId)
			uCache.Add(r.UserId)
		}
	}

	// TODO: extract @mentions to local users and sync those as well?

	users := make([]*model.User, 0)

	for _, id := range userIds {
		user, err := scs.server.GetStore().User().Get(context.Background(), id)
		if err == nil {
			if sync, err2 := scs.shouldUserSync(user, rc); err2 != nil {
				scs.server.GetLogger().Log(mlog.LvlSharedChannelServiceError, "Could not find user for post",
					mlog.String("user_id", id),
					mlog.Err(err2))
				continue
			} else if sync {
				users = append(users, sanitizeUserForSync(user))
			}
		} else {
			scs.server.GetLogger().Log(mlog.LvlSharedChannelServiceError, "Error checking if user should sync",
				mlog.String("user_id", id),
				mlog.Err(err))
		}
	}
	return users
}

func sanitizeUserForSync(user *model.User) *model.User {
	user.Password = model.NewId()
	user.AuthData = nil
	user.AuthService = ""
	user.Roles = "system_user"
	user.AllowMarketing = false
	user.Props = model.StringMap{}
	user.NotifyProps = model.StringMap{}
	user.LastPasswordUpdate = 0
	user.LastPictureUpdate = 0
	user.FailedAttempts = 0
	user.MfaActive = false
	user.MfaSecret = ""

	return user
}

// shouldUserSync determines if a user needs to be synchronized.
// User should be synchronized if it has no entry in the SharedChannelUsers table,
// or there is an entry but the LastSyncAt is less than user.UpdateAt
func (scs *Service) shouldUserSync(user *model.User, rc *model.RemoteCluster) (bool, error) {
	// don't sync users with the remote they originated from.
	if user.RemoteId != nil && *user.RemoteId == rc.RemoteId {
		return false, nil
	}

	scu, err := scs.server.GetStore().SharedChannel().GetUser(user.Id, rc.RemoteId)
	if err != nil {
		if _, ok := err.(errNotFound); !ok {
			return false, err
		}

		// user not in the SharedChannelUsers table, so we must add them.
		scu = &model.SharedChannelUser{
			UserId:   user.Id,
			RemoteId: rc.RemoteId,
		}
		if _, err = scs.server.GetStore().SharedChannel().SaveUser(scu); err != nil {
			scs.server.GetLogger().Log(mlog.LvlSharedChannelServiceError, "Error adding user to shared channel users",
				mlog.String("remote_id", rc.RemoteId),
				mlog.String("user_id", user.Id),
			)
		}
	} else if scu.LastSyncAt >= user.UpdateAt {
		return false, nil
	}
	return true, nil
}