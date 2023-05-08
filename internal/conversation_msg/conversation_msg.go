// Copyright © 2023 OpenIM SDK. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package conversation_msg

import (
	"context"
	"encoding/json"
	"github.com/OpenIMSDK/Open-IM-Server/pkg/common/log"
	"github.com/OpenIMSDK/Open-IM-Server/pkg/proto/sdkws"
	"open_im_sdk/internal/business"
	"open_im_sdk/internal/cache"
	"open_im_sdk/internal/friend"
	"open_im_sdk/internal/full"
	"open_im_sdk/internal/group"
	"open_im_sdk/internal/interaction"
	"open_im_sdk/internal/signaling"
	"open_im_sdk/internal/user"
	workMoments "open_im_sdk/internal/work_moments"
	"open_im_sdk/open_im_sdk_callback"
	"open_im_sdk/pkg/ccontext"
	"open_im_sdk/pkg/common"
	"open_im_sdk/pkg/constant"
	"open_im_sdk/pkg/db/db_interface"
	"open_im_sdk/pkg/db/model_struct"
	sdk "open_im_sdk/pkg/sdk_params_callback"
	"open_im_sdk/pkg/server_api_params"
	"open_im_sdk/pkg/syncer"

	"open_im_sdk/pkg/utils"
	"open_im_sdk/sdk_struct"
	"sort"
	"sync"
	"time"

	"github.com/golang/protobuf/proto"
	"github.com/jinzhu/copier"
)

var SearchContentType = []int{constant.Text, constant.AtText, constant.File}

type Conversation struct {
	*interaction.LongConnMgr
	conversationSyncer   *syncer.Syncer[*model_struct.LocalConversation, string]
	db                   db_interface.DataBase
	ConversationListener open_im_sdk_callback.OnConversationListener
	msgListener          open_im_sdk_callback.OnAdvancedMsgListener
	msgKvListener        open_im_sdk_callback.OnMessageKvInfoListener
	batchMsgListener     open_im_sdk_callback.OnBatchMsgListener
	recvCH               chan common.Cmd2Value
	loginUserID          string

	platformID  int32
	DataDir     string
	friend      *friend.Friend
	group       *group.Group
	user        *user.User
	signaling   *signaling.LiveSignaling
	workMoments *workMoments.WorkMoments
	business    *business.Business

	cache          *cache.Cache
	full           *full.Full
	tempMessageMap sync.Map
	encryptionKey  string

	id2MinSeq            map[string]int64
	IsExternalExtensions bool

	listenerForService open_im_sdk_callback.OnListenerForService
}

func (c *Conversation) SetListenerForService(listener open_im_sdk_callback.OnListenerForService) {
	c.listenerForService = listener
}

func (c *Conversation) MsgListener() open_im_sdk_callback.OnAdvancedMsgListener {
	return c.msgListener
}

func (c *Conversation) SetSignaling(signaling *signaling.LiveSignaling) {
	c.signaling = signaling
}

func (c *Conversation) SetMsgListener(msgListener open_im_sdk_callback.OnAdvancedMsgListener) {
	c.msgListener = msgListener
}
func (c *Conversation) SetMsgKvListener(msgKvListener open_im_sdk_callback.OnMessageKvInfoListener) {
	c.msgKvListener = msgKvListener
}
func (c *Conversation) SetBatchMsgListener(batchMsgListener open_im_sdk_callback.OnBatchMsgListener) {
	c.batchMsgListener = batchMsgListener
}

func NewConversation(ctx context.Context, longConnMgr *interaction.LongConnMgr, db db_interface.DataBase,
	ch chan common.Cmd2Value,
	friend *friend.Friend, group *group.Group, user *user.User,
	conversationListener open_im_sdk_callback.OnConversationListener,
	msgListener open_im_sdk_callback.OnAdvancedMsgListener, signaling *signaling.LiveSignaling,
	workMoments *workMoments.WorkMoments, business *business.Business, cache *cache.Cache, full *full.Full, id2MinSeq map[string]int64) *Conversation {
	info := ccontext.Info(ctx)
	n := &Conversation{db: db,
		LongConnMgr:          longConnMgr,
		recvCH:               ch,
		loginUserID:          info.UserID(),
		platformID:           info.Platform(),
		DataDir:              info.DataDir(),
		friend:               friend,
		group:                group,
		user:                 user,
		signaling:            signaling,
		workMoments:          workMoments,
		full:                 full,
		id2MinSeq:            id2MinSeq,
		encryptionKey:        info.EncryptionKey(),
		business:             business,
		IsExternalExtensions: info.IsExternalExtensions(),
	}
	n.SetMsgListener(msgListener)
	n.SetConversationListener(conversationListener)
	n.initSyncer()
	n.cache = cache
	return n
}

func (c *Conversation) initSyncer() {
	c.conversationSyncer = syncer.New(
		func(ctx context.Context, value *model_struct.LocalConversation) error {
			return c.db.InsertConversation(ctx, value)
		},
		func(ctx context.Context, value *model_struct.LocalConversation) error {
			return c.db.DeleteConversation(ctx, value.ConversationID)
		},
		func(ctx context.Context, serverConversation, localConversation *model_struct.LocalConversation) error {
			return c.db.UpdateConversation(ctx, serverConversation)
		},
		func(value *model_struct.LocalConversation) string {
			return value.ConversationID
		},
		nil,
		nil,
	)
}

func (c *Conversation) GetCh() chan common.Cmd2Value {
	return c.recvCH
}

func (c *Conversation) doMsgNew(c2v common.Cmd2Value) {
	//operationID := c2v.Value.(sdk_struct.CmdNewMsgComeToConversation).OperationID
	allMsg := c2v.Value.(sdk_struct.CmdNewMsgComeToConversation).Msgs
	ctx := c2v.Ctx
	var isTriggerUnReadCount bool
	var insertMsg, updateMsg []*model_struct.LocalChatLog
	var exceptionMsg []*model_struct.LocalErrChatLog
	var unreadMessages []*model_struct.LocalConversationUnreadMessage
	var newMessages, msgReadList, groupMsgReadList, msgRevokeList, newMsgRevokeList, reactionMsgModifierList, reactionMsgDeleterList sdk_struct.NewMsgList
	var isUnreadCount, isConversationUpdate, isHistory, isNotPrivate, isSenderConversationUpdate, isSenderNotificationPush bool
	conversationChangedSet := make(map[string]*model_struct.LocalConversation)
	newConversationSet := make(map[string]*model_struct.LocalConversation)
	conversationSet := make(map[string]*model_struct.LocalConversation)
	phConversationChangedSet := make(map[string]*model_struct.LocalConversation)
	phNewConversationSet := make(map[string]*model_struct.LocalConversation)
	log.ZDebug(ctx, "do Msg come here", "len", len(allMsg), "ch len", len(c.GetCh()))
	b := time.Now()
	for _, msgs := range allMsg {
		for _, v := range msgs.Msgs {
			log.ZDebug(ctx, "do Msg come here", "loginUserID", c.loginUserID, "msg", v)
			isHistory = utils.GetSwitchFromOptions(v.Options, constant.IsHistory)
			isUnreadCount = utils.GetSwitchFromOptions(v.Options, constant.IsUnreadCount)
			isConversationUpdate = utils.GetSwitchFromOptions(v.Options, constant.IsConversationUpdate)
			isNotPrivate = utils.GetSwitchFromOptions(v.Options, constant.IsNotPrivate)
			isSenderConversationUpdate = utils.GetSwitchFromOptions(v.Options, constant.IsSenderConversationUpdate)
			isSenderNotificationPush = utils.GetSwitchFromOptions(v.Options, constant.IsSenderNotificationPush)
			msg := new(sdk_struct.MsgStruct)
			copier.Copy(msg, v)
			if v.OfflinePushInfo != nil {
				msg.OfflinePush = *v.OfflinePushInfo
			}
			msg.Content = string(v.Content)
			//var tips sdkws.TipsComm
			//if v.ContentType >= constant.NotificationBegin && v.ContentType <= constant.NotificationEnd {
			//	_ = proto.Unmarshal(v.Content, &tips)
			//	marshaler := jsonpb.Marshaler{
			//		OrigName:     true,
			//		EnumsAsInts:  false,
			//		EmitDefaults: false,
			//	}
			//	msg.Content, _ = marshaler.MarshalToString(&tips)
			//} else {
			//	msg.Content = string(v.Content)
			//}
			//When the message has been marked and deleted by the cloud, it is directly inserted locally without any conversation and message update.
			if msg.Status == constant.MsgStatusHasDeleted {
				insertMsg = append(insertMsg, c.msgStructToLocalChatLog(msg))
				continue
			}
			msg.Status = constant.MsgStatusSendSuccess
			msg.IsRead = false
			//De-analyze data
			err := c.msgHandleByContentType(msg)
			if err != nil {
				log.ZError(ctx, "Parsing data error:", err, "type: ", msg.ContentType)
				continue
			}
			if !isSenderNotificationPush {
				msg.AttachedInfoElem.NotSenderNotificationPush = true
				msg.AttachedInfo = utils.StructToJsonString(msg.AttachedInfoElem)
			}
			if !isNotPrivate {
				msg.AttachedInfoElem.IsPrivateChat = true
				msg.AttachedInfo = utils.StructToJsonString(msg.AttachedInfoElem)
			}
			if msg.ClientMsgID == "" {
				exceptionMsg = append(exceptionMsg, c.msgStructToLocalErrChatLog(msg))
				continue
			}
			switch {
			case v.ContentType == constant.ConversationChangeNotification || v.ContentType == constant.ConversationPrivateChatNotification:
				c.DoNotification(ctx, v)
			case v.ContentType == constant.MsgDeleteNotification:
			case v.ContentType == constant.SuperGroupUpdateNotification:
				c.full.SuperGroup.DoNotification(ctx, v, c.GetCh())
				continue
			case v.ContentType == constant.ConversationUnreadNotification:
				var unreadArgs sdkws.ConversationUpdateTips
				_ = proto.Unmarshal([]byte(msg.Content), &unreadArgs)
				for _, v := range unreadArgs.ConversationIDList {
					c.doUpdateConversation(common.Cmd2Value{Value: common.UpdateConNode{ConID: v, Action: constant.UnreadCountSetZero}})
					c.db.DeleteConversationUnreadMessageList(ctx, v, unreadArgs.UpdateUnreadCountTime)
				}
				c.doUpdateConversation(common.Cmd2Value{Value: common.UpdateConNode{Action: constant.ConChange, Args: unreadArgs.ConversationIDList}})
				continue
			case v.ContentType == constant.BusinessNotification:
				c.business.DoNotification(ctx, msg.Content)
				continue
			}

			switch v.SessionType {
			case constant.SingleChatType:
				if v.ContentType > constant.FriendNotificationBegin && v.ContentType < constant.FriendNotificationEnd {
					c.friend.DoNotification(ctx, v)
				} else if v.ContentType > constant.UserNotificationBegin && v.ContentType < constant.UserNotificationEnd {
					c.user.DoNotification(ctx, v)
					//	c.friend.DoNotification(v, c.GetCh())
				} else if v.ContentType == constant.GroupApplicationRejectedNotification ||
					v.ContentType == constant.GroupApplicationAcceptedNotification ||
					v.ContentType == constant.JoinGroupApplicationNotification {
					c.group.DoNotification(ctx, v)
				} else if v.ContentType > constant.SignalingNotificationBegin && v.ContentType < constant.SignalingNotificationEnd {
					c.signaling.DoNotification(ctx, v, c.GetCh())
					continue
				} else if v.ContentType == constant.WorkMomentNotification {
					c.workMoments.DoNotification(ctx, msg.Content)
				}
			case constant.GroupChatType, constant.SuperGroupChatType:
				if v.ContentType > constant.GroupNotificationBegin && v.ContentType < constant.GroupNotificationEnd {
					c.group.DoNotification(ctx, v)
					log.ZInfo(ctx, "DoGroupMsg SingleChatType", v)
				} else if v.ContentType > constant.SignalingNotificationBegin && v.ContentType < constant.SignalingNotificationEnd {
					log.ZInfo(ctx, "signaling DoNotification ", v)
					c.signaling.DoNotification(ctx, v, c.GetCh())
					continue
				}
			}
			if v.SendID == c.loginUserID { //seq
				// Messages sent by myself  //if  sent through  this terminal
				m, err := c.db.GetMessageController(ctx, msg)
				if err == nil {
					log.ZInfo(ctx, "have message", msg.Seq, msg.ServerMsgID, msg.ClientMsgID, *msg)
					if m.Seq == 0 {
						if !isConversationUpdate {
							msg.Status = constant.MsgStatusFiltered
						}
						updateMsg = append(updateMsg, c.msgStructToLocalChatLog(msg))
					} else {
						exceptionMsg = append(exceptionMsg, c.msgStructToLocalErrChatLog(msg))
					}
				} else {
					log.ZInfo(ctx, "sync message", msg.Seq, msg.ServerMsgID, msg.ClientMsgID, *msg)
					lc := model_struct.LocalConversation{
						ConversationType:  v.SessionType,
						LatestMsg:         utils.StructToJsonString(msg),
						LatestMsgSendTime: msg.SendTime,
					}
					switch v.SessionType {
					case constant.SingleChatType:
						lc.ConversationID = utils.GetConversationIDBySessionType(v.RecvID, constant.SingleChatType)
						lc.UserID = v.RecvID
					case constant.GroupChatType:
						lc.GroupID = v.GroupID
						lc.ConversationID = utils.GetConversationIDBySessionType(lc.GroupID, constant.GroupChatType)
					case constant.SuperGroupChatType:
						lc.GroupID = v.GroupID
						lc.ConversationID = utils.GetConversationIDBySessionType(lc.GroupID, constant.SuperGroupChatType)
					}
					if isConversationUpdate {
						if isSenderConversationUpdate {
							log.ZDebug(ctx, "updateConversation msg", v, lc)
							c.updateConversation(&lc, conversationSet)
						}
						newMessages = append(newMessages, msg)
					} else {
						msg.Status = constant.MsgStatusFiltered
					}
					if isHistory {
						insertMsg = append(insertMsg, c.msgStructToLocalChatLog(msg))
					}
					switch msg.ContentType {
					case constant.Revoke:
						msgRevokeList = append(msgRevokeList, msg)
					case constant.HasReadReceipt:
						msgReadList = append(msgReadList, msg)
					case constant.GroupHasReadReceipt:
						groupMsgReadList = append(groupMsgReadList, msg)
					case constant.AdvancedRevoke:
						newMsgRevokeList = append(newMsgRevokeList, msg)
						newMessages = removeElementInList(newMessages, msg)
					case constant.ReactionMessageModifier:
						reactionMsgModifierList = append(reactionMsgModifierList, msg)
					case constant.ReactionMessageDeleter:
						reactionMsgDeleterList = append(reactionMsgDeleterList, msg)
					default:
					}
				}
			} else { //Sent by others
				if _, err := c.db.GetMessageController(ctx, msg); err != nil { //Deduplication operation
					lc := model_struct.LocalConversation{
						ConversationType:  v.SessionType,
						LatestMsg:         utils.StructToJsonString(msg),
						LatestMsgSendTime: msg.SendTime,
					}
					switch v.SessionType {
					case constant.SingleChatType:
						lc.ConversationID = utils.GetConversationIDBySessionType(v.SendID, constant.SingleChatType)
						lc.UserID = v.SendID
						lc.ShowName = msg.SenderNickname
						lc.FaceURL = msg.SenderFaceURL
					case constant.GroupChatType:
						lc.GroupID = v.GroupID
						lc.ConversationID = utils.GetConversationIDBySessionType(lc.GroupID, constant.GroupChatType)
					case constant.SuperGroupChatType:
						lc.GroupID = v.GroupID
						lc.ConversationID = utils.GetConversationIDBySessionType(lc.GroupID, constant.SuperGroupChatType)
						//faceUrl, name, err := u.getGroupNameAndFaceUrlByUid(c.GroupID)
						//if err != nil {
						//	utils.sdkLog("getGroupNameAndFaceUrlByUid err:", err)
						//} else {
						//	c.ShowName = name
						//	c.FaceURL = faceUrl
						//}
					case constant.NotificationChatType:
						lc.ConversationID = utils.GetConversationIDBySessionType(v.SendID, constant.NotificationChatType)
						lc.UserID = v.SendID
					}
					if isUnreadCount {
						cacheConversation := c.cache.GetConversation(lc.ConversationID)
						if msg.SendTime > cacheConversation.UpdateUnreadCountTime {
							isTriggerUnReadCount = true
							lc.UnreadCount = 1
							tempUnreadMessages := model_struct.LocalConversationUnreadMessage{ConversationID: lc.ConversationID, ClientMsgID: msg.ClientMsgID, SendTime: msg.SendTime}
							unreadMessages = append(unreadMessages, &tempUnreadMessages)
						}
					}
					if isConversationUpdate {
						c.updateConversation(&lc, conversationSet)
						newMessages = append(newMessages, msg)
					} else {
						msg.Status = constant.MsgStatusFiltered
					}
					if isHistory {
						log.ZDebug(ctx, "trigger msg is ", msg.SenderNickname, msg.SenderFaceURL)
						insertMsg = append(insertMsg, c.msgStructToLocalChatLog(msg))
					}
					switch msg.ContentType {
					case constant.Revoke:
						msgRevokeList = append(msgRevokeList, msg)
					case constant.HasReadReceipt:
						msgReadList = append(msgReadList, msg)
					case constant.GroupHasReadReceipt:
						groupMsgReadList = append(groupMsgReadList, msg)
					case constant.Typing:
						newMessages = append(newMessages, msg)
					case constant.CustomMsgOnlineOnly:
						newMessages = append(newMessages, msg)
					case constant.CustomMsgNotTriggerConversation:
						newMessages = append(newMessages, msg)
					case constant.OANotification:
						if !isConversationUpdate {
							newMessages = append(newMessages, msg)
						}
					case constant.AdvancedRevoke:
						newMsgRevokeList = append(newMsgRevokeList, msg)
						newMessages = removeElementInList(newMessages, msg)
					case constant.ReactionMessageModifier:
						reactionMsgModifierList = append(reactionMsgModifierList, msg)
					case constant.ReactionMessageDeleter:
						reactionMsgDeleterList = append(reactionMsgDeleterList, msg)
					default:
					}

				} else {
					exceptionMsg = append(exceptionMsg, c.msgStructToLocalErrChatLog(msg))
					log.ZWarn(ctx, "Deduplication operation ", nil, "msg", *c.msgStructToLocalErrChatLog(msg))
				}
			}
		}
	}

	list, err := c.db.GetAllConversationListDB(ctx)
	if err != nil {
		log.ZError(ctx, "GetAllConversationListDB", err)
	}
	m := make(map[string]*model_struct.LocalConversation)
	listToMap(list, m)
	log.ZDebug(ctx, "listToMap: ", list, conversationSet)
	c.diff(ctx, m, conversationSet, conversationChangedSet, newConversationSet)
	log.ZInfo(ctx, "trigger map is :", "newConversations", newConversationSet, "changedConversations", conversationChangedSet)

	//seq sync message update
	if err := c.db.BatchUpdateMessageList(ctx, updateMsg); err != nil {
		log.ZError(ctx, "sync seq normal message err  :", err)
	}

	//Normal message storage
	if err := c.db.BatchInsertMessageListController(ctx, insertMsg); err != nil {
		log.ZError(ctx, "insert GetMessage detail err:", err, len(insertMsg))
		for _, v := range insertMsg {
			e := c.db.InsertMessageController(ctx, v)
			if e != nil {
				errChatLog := &model_struct.LocalErrChatLog{}
				copier.Copy(errChatLog, v)
				exceptionMsg = append(exceptionMsg, errChatLog)
				log.ZWarn(ctx, "InsertMessage operation", err, "chatErrLog", errChatLog, "chatLog", v)
			}
		}
	}

	//Exception message storage
	log.ZWarn(ctx, "exceptionMsgs", nil, "msgs", exceptionMsg)

	if err := c.db.BatchInsertExceptionMsgController(ctx, exceptionMsg); err != nil {
		log.ZError(ctx, "insert err message err  :", err)

	}
	hList, _ := c.db.GetHiddenConversationList(ctx)
	for _, v := range hList {
		if nc, ok := newConversationSet[v.ConversationID]; ok {
			phConversationChangedSet[v.ConversationID] = nc
			nc.RecvMsgOpt = v.RecvMsgOpt
			nc.GroupAtType = v.GroupAtType
			nc.IsPinned = v.IsPinned
			nc.IsPrivateChat = v.IsPrivateChat
			if nc.IsPrivateChat {
				nc.BurnDuration = v.BurnDuration
			}
			nc.IsNotInGroup = v.IsNotInGroup
			nc.AttachedInfo = v.AttachedInfo
			nc.Ex = v.Ex
		}
	}

	for k, v := range newConversationSet {
		if _, ok := phConversationChangedSet[v.ConversationID]; !ok {
			phNewConversationSet[k] = v
		}
	}
	//Changed conversation storage

	if err := c.db.BatchUpdateConversationList(ctx, append(mapConversationToList(conversationChangedSet), mapConversationToList(phConversationChangedSet)...)); err != nil {
		log.ZError(ctx, "insert changed conversation err :", err)
	}
	//New conversation storage

	if err := c.db.BatchInsertConversationList(ctx, mapConversationToList(phNewConversationSet)); err != nil {
		log.ZError(ctx, "insert new conversation err:", err)
	}

	if err := c.db.BatchInsertConversationUnreadMessageList(ctx, unreadMessages); err != nil {
		log.ZError(ctx, "insert BatchInsertConversationUnreadMessageList err:", err)
	}
	c.doMsgReadState(ctx, msgReadList)

	c.DoGroupMsgReadState(ctx, groupMsgReadList)
	c.revokeMessage(ctx, msgRevokeList)
	if c.batchMsgListener != nil {
		c.batchNewMessages(ctx, newMessages)
	} else {
		c.newMessage(newMessages)
	}
	c.newRevokeMessage(ctx, newMsgRevokeList)
	c.doReactionMsgModifier(ctx, reactionMsgModifierList)
	c.doReactionMsgDeleter(ctx, reactionMsgDeleterList)
	//log.Info(operationID, "trigger map is :", newConversationSet, conversationChangedSet)
	if len(newConversationSet) > 0 {
		c.doUpdateConversation(common.Cmd2Value{Value: common.UpdateConNode{Action: constant.NewConDirect, Args: utils.StructToJsonString(mapConversationToList(newConversationSet))}})

	}
	if len(conversationChangedSet) > 0 {
		c.doUpdateConversation(common.Cmd2Value{Value: common.UpdateConNode{Action: constant.ConChangeDirect, Args: utils.StructToJsonString(mapConversationToList(conversationChangedSet))}})
	}

	if isTriggerUnReadCount {
		c.doUpdateConversation(common.Cmd2Value{Value: common.UpdateConNode{Action: constant.TotalUnreadMessageChanged, Args: ""}})
	}

	log.ZDebug(ctx, "insert msg, total cost time: ", time.Since(b), "len:  ", len(allMsg))
}

func (c *Conversation) doSuperGroupMsgNew(c2v common.Cmd2Value) {
	//operationID := c2v.Value.(sdk_struct.CmdNewMsgComeToConversation).OperationID
	allMsg := c2v.Value.(sdk_struct.CmdNewMsgComeToConversation).Msgs
	ctx := c2v.Ctx
	var isTriggerUnReadCount bool
	var insertMsg, updateMsg, specialUpdateMsg []*model_struct.LocalChatLog
	var exceptionMsg []*model_struct.LocalErrChatLog
	var unreadMessages []*model_struct.LocalConversationUnreadMessage
	var newMessages, msgReadList, groupMsgReadList, msgRevokeList, newMsgRevokeList, reactionMsgModifierList, reactionMsgDeleterList sdk_struct.NewMsgList
	var isUnreadCount, isConversationUpdate, isHistory, isNotPrivate, isSenderConversationUpdate, isSenderNotificationPush bool
	conversationChangedSet := make(map[string]*model_struct.LocalConversation)
	newConversationSet := make(map[string]*model_struct.LocalConversation)
	conversationSet := make(map[string]*model_struct.LocalConversation)
	phConversationChangedSet := make(map[string]*model_struct.LocalConversation)
	phNewConversationSet := make(map[string]*model_struct.LocalConversation)
	log.ZDebug(ctx, "do Msg come here", "len", len(allMsg))
	for _, msgs := range allMsg {
		for _, v := range msgs.Msgs {
			isHistory = utils.GetSwitchFromOptions(v.Options, constant.IsHistory)
			isUnreadCount = utils.GetSwitchFromOptions(v.Options, constant.IsUnreadCount)
			isConversationUpdate = utils.GetSwitchFromOptions(v.Options, constant.IsConversationUpdate)
			isNotPrivate = utils.GetSwitchFromOptions(v.Options, constant.IsNotPrivate)
			isSenderConversationUpdate = utils.GetSwitchFromOptions(v.Options, constant.IsSenderConversationUpdate)
			isSenderNotificationPush = utils.GetSwitchFromOptions(v.Options, constant.IsSenderNotificationPush)
			msg := new(sdk_struct.MsgStruct)
			copier.Copy(msg, v)
			if v.OfflinePushInfo != nil {
				msg.OfflinePush = *v.OfflinePushInfo
			}
			msg.Content = string(v.Content)
			//var tips sdkws.TipsComm
			//if v.ContentType >= constant.NotificationBegin && v.ContentType <= constant.NotificationEnd {
			//	_ = proto.Unmarshal(v.Content, &tips)
			//	marshaler := jsonpb.Marshaler{
			//		OrigName:     true,
			//		EnumsAsInts:  false,
			//		EmitDefaults: false,
			//	}
			//	msg.Content, _ = marshaler.MarshalToString(&tips)
			//} else {
			//	msg.Content = string(v.Content)
			//}
			//When the message has been marked and deleted by the cloud, it is directly inserted locally without any conversation and message update.
			if msg.Status == constant.MsgStatusHasDeleted {
				insertMsg = append(insertMsg, c.msgStructToLocalChatLog(msg))
				continue
			}
			msg.Status = constant.MsgStatusSendSuccess
			msg.IsRead = false
			//		log.Info(operationID, "new msg, seq, ServerMsgID, ClientMsgID", msg.Seq, msg.ServerMsgID, msg.ClientMsgID)
			//De-analyze data
			err := c.msgHandleByContentType(msg)
			if err != nil {
				log.ZError(ctx, "msgHandleByContentType error:", err, "type: ", msg.ContentType, "msg", msg)
				continue
			}
			if !isSenderNotificationPush {
				msg.AttachedInfoElem.NotSenderNotificationPush = true
				msg.AttachedInfo = utils.StructToJsonString(msg.AttachedInfoElem)
			}
			if !isNotPrivate {
				msg.AttachedInfoElem.IsPrivateChat = true
				msg.AttachedInfo = utils.StructToJsonString(msg.AttachedInfoElem)
			}
			if msg.ClientMsgID == "" {
				exceptionMsg = append(exceptionMsg, c.msgStructToLocalErrChatLog(msg))
				continue
			}
			switch {
			case v.ContentType == constant.ConversationChangeNotification || v.ContentType == constant.ConversationPrivateChatNotification:
				c.DoNotification(ctx, v)
			case v.ContentType == constant.SuperGroupUpdateNotification:
				c.full.SuperGroup.DoNotification(ctx, v, c.GetCh())
			case v.ContentType == constant.ConversationUnreadNotification:
				var unreadArgs sdkws.ConversationUpdateTips
				_ = proto.Unmarshal([]byte(msg.Content), &unreadArgs)
				for _, v := range unreadArgs.ConversationIDList {
					c.doUpdateConversation(common.Cmd2Value{Value: common.UpdateConNode{ConID: v, Action: constant.UnreadCountSetZero}})
					c.db.DeleteConversationUnreadMessageList(ctx, v, unreadArgs.UpdateUnreadCountTime)
				}
				c.doUpdateConversation(common.Cmd2Value{Value: common.UpdateConNode{Action: constant.ConChange, Args: unreadArgs.ConversationIDList}})
				continue
			case v.ContentType == constant.BusinessNotification:
				c.business.DoNotification(ctx, msg.Content)
				continue
			}
			switch v.SessionType {
			case constant.SingleChatType:
				if v.ContentType > constant.FriendNotificationBegin && v.ContentType < constant.FriendNotificationEnd {
					c.friend.DoNotification(ctx, v)
				} else if v.ContentType > constant.UserNotificationBegin && v.ContentType < constant.UserNotificationEnd {
					c.user.DoNotification(ctx, v)
					//c.friend.DoNotification(v, c.GetCh())
				} else if v.ContentType == constant.GroupApplicationRejectedNotification ||
					v.ContentType == constant.GroupApplicationAcceptedNotification ||
					v.ContentType == constant.JoinGroupApplicationNotification {
					c.group.DoNotification(ctx, v)
				} else if v.ContentType > constant.SignalingNotificationBegin && v.ContentType < constant.SignalingNotificationEnd {
					c.signaling.DoNotification(ctx, v, c.GetCh())
					continue
				} else if v.ContentType == constant.WorkMomentNotification {
					c.workMoments.DoNotification(ctx, msg.Content)
				}
			case constant.GroupChatType, constant.SuperGroupChatType:
				if v.ContentType > constant.GroupNotificationBegin && v.ContentType < constant.GroupNotificationEnd {
					c.group.DoNotification(ctx, v)
				} else if v.ContentType > constant.SignalingNotificationBegin && v.ContentType < constant.SignalingNotificationEnd {
					c.signaling.DoNotification(ctx, v, c.GetCh())
					continue
				}
			}
			if v.SendID == c.loginUserID { //seq
				// Messages sent by myself  //if  sent through  this terminal
				m, err := c.db.GetMessageController(ctx, msg)
				if err == nil {
					if m.Seq == 0 {
						if m.CreateTime == 0 {
							specialUpdateMsg = append(specialUpdateMsg, c.msgStructToLocalChatLog(msg))
						} else {
							if !isConversationUpdate {
								msg.Status = constant.MsgStatusFiltered
							}
							updateMsg = append(updateMsg, c.msgStructToLocalChatLog(msg))
						}
					} else {
						exceptionMsg = append(exceptionMsg, c.msgStructToLocalErrChatLog(msg))
					}
				} else { //      send through  other terminal
					lc := model_struct.LocalConversation{
						ConversationType:  v.SessionType,
						LatestMsg:         utils.StructToJsonString(msg),
						LatestMsgSendTime: msg.SendTime,
					}
					switch v.SessionType {
					case constant.SingleChatType:
						lc.ConversationID = utils.GetConversationIDBySessionType(v.RecvID, constant.SingleChatType)
						lc.UserID = v.RecvID
						//localUserInfo,_ := c.user.GetLoginUser()
						//c.FaceURL = localUserInfo.FaceUrl
						//c.ShowName = localUserInfo.Nickname
					case constant.GroupChatType:
						lc.GroupID = v.GroupID
						lc.ConversationID = utils.GetConversationIDBySessionType(lc.GroupID, constant.GroupChatType)
					case constant.SuperGroupChatType:
						lc.GroupID = v.GroupID
						lc.ConversationID = utils.GetConversationIDBySessionType(lc.GroupID, constant.SuperGroupChatType)
						//faceUrl, name, err := u.getGroupNameAndFaceUrlByUid(c.GroupID)
						//if err != nil {
						//	utils.sdkLog("getGroupNameAndFaceUrlByUid err:", err)
						//} else {
						//	c.ShowName = name
						//	c.FaceURL = faceUrl
						//}

					}
					if isConversationUpdate {
						if isSenderConversationUpdate {
							c.updateConversation(&lc, conversationSet)
						}
						newMessages = append(newMessages, msg)
					} else {
						msg.Status = constant.MsgStatusFiltered
					}
					if isHistory {
						insertMsg = append(insertMsg, c.msgStructToLocalChatLog(msg))
					}
					switch msg.ContentType {
					case constant.Revoke:
						msgRevokeList = append(msgRevokeList, msg)
					case constant.HasReadReceipt:
						msgReadList = append(msgReadList, msg)
					case constant.GroupHasReadReceipt:
						groupMsgReadList = append(groupMsgReadList, msg)
					case constant.AdvancedRevoke:
						newMsgRevokeList = append(newMsgRevokeList, msg)
						newMessages = removeElementInList(newMessages, msg)
					case constant.ReactionMessageModifier:
						reactionMsgModifierList = append(reactionMsgModifierList, msg)
					case constant.ReactionMessageDeleter:
						reactionMsgDeleterList = append(reactionMsgDeleterList, msg)
					default:
					}
				}
			} else { //Sent by others
				if oldMessage, err := c.db.GetMessageController(ctx, msg); err != nil { //Deduplication operation
					lc := model_struct.LocalConversation{
						ConversationType:  v.SessionType,
						LatestMsg:         utils.StructToJsonString(msg),
						LatestMsgSendTime: msg.SendTime,
					}
					switch v.SessionType {
					case constant.SingleChatType:
						lc.ConversationID = utils.GetConversationIDBySessionType(v.SendID, constant.SingleChatType)
						lc.UserID = v.SendID
						lc.ShowName = msg.SenderNickname
						lc.FaceURL = msg.SenderFaceURL
					case constant.GroupChatType:
						lc.GroupID = v.GroupID
						lc.ConversationID = utils.GetConversationIDBySessionType(lc.GroupID, constant.GroupChatType)
					case constant.SuperGroupChatType:
						lc.GroupID = v.GroupID
						lc.ConversationID = utils.GetConversationIDBySessionType(lc.GroupID, constant.SuperGroupChatType)
						//faceUrl, name, err := u.getGroupNameAndFaceUrlByUid(c.GroupID)
						//if err != nil {
						//	utils.sdkLog("getGroupNameAndFaceUrlByUid err:", err)
						//} else {
						//	c.ShowName = name
						//	c.FaceURL = faceUrl
						//}
					case constant.NotificationChatType:
						lc.ConversationID = utils.GetConversationIDBySessionType(v.SendID, constant.NotificationChatType)
						lc.UserID = v.SendID
					}
					if isUnreadCount {
						cacheConversation := c.cache.GetConversation(lc.ConversationID)
						if msg.SendTime > cacheConversation.UpdateUnreadCountTime {
							isTriggerUnReadCount = true
							lc.UnreadCount = 1
							tempUnreadMessages := model_struct.LocalConversationUnreadMessage{ConversationID: lc.ConversationID, ClientMsgID: msg.ClientMsgID, SendTime: msg.SendTime}
							unreadMessages = append(unreadMessages, &tempUnreadMessages)
						}
					}
					if isConversationUpdate {
						c.updateConversation(&lc, conversationSet)
						newMessages = append(newMessages, msg)
					} else {
						msg.Status = constant.MsgStatusFiltered
					}
					if isHistory {
						insertMsg = append(insertMsg, c.msgStructToLocalChatLog(msg))
					}
					switch msg.ContentType {
					case constant.Revoke:
						msgRevokeList = append(msgRevokeList, msg)
					case constant.HasReadReceipt:
						msgReadList = append(msgReadList, msg)
					case constant.GroupHasReadReceipt:
						groupMsgReadList = append(groupMsgReadList, msg)
					case constant.CustomMsgOnlineOnly:
						newMessages = append(newMessages, msg)
					case constant.CustomMsgNotTriggerConversation:
						newMessages = append(newMessages, msg)
					case constant.OANotification:
						if !isConversationUpdate {
							newMessages = append(newMessages, msg)
						}
					case constant.Typing:
						newMessages = append(newMessages, msg)
					case constant.AdvancedRevoke:
						newMsgRevokeList = append(newMsgRevokeList, msg)
						newMessages = removeElementInList(newMessages, msg)
					case constant.ReactionMessageModifier:
						reactionMsgModifierList = append(reactionMsgModifierList, msg)
					case constant.ReactionMessageDeleter:
						reactionMsgDeleterList = append(reactionMsgDeleterList, msg)
					default:
					}

				} else {
					if oldMessage.Seq == 0 {
						specialUpdateMsg = append(specialUpdateMsg, c.msgStructToLocalChatLog(msg))
					} else {
						exceptionMsg = append(exceptionMsg, c.msgStructToLocalErrChatLog(msg))
						log.ZWarn(ctx, "Deduplication operation", nil, "msg", *c.msgStructToLocalErrChatLog(msg))
					}
				}
			}
		}
	}

	list, err := c.db.GetAllConversationListDB(ctx)
	if err != nil {
		log.ZError(ctx, "GetAllConversationListDB", err)
	}
	m := make(map[string]*model_struct.LocalConversation)
	listToMap(list, m)
	c.diff(ctx, m, conversationSet, conversationChangedSet, newConversationSet)
	if err := c.db.BatchSpecialUpdateMessageList(ctx, specialUpdateMsg); err != nil {
		log.ZError(ctx, "sync seq normal message", err)
	}
	if err := c.db.BatchUpdateMessageList(ctx, updateMsg); err != nil {
		log.ZError(ctx, "sync seq normal message", err)
	}

	//Normal message storage
	if err := c.db.BatchInsertMessageListController(ctx, insertMsg); err != nil {
		log.ZError(ctx, "insert GetMessage detail err:", err, "insertMsg", len(insertMsg))
		for _, v := range insertMsg {
			err := c.db.InsertMessageController(ctx, v)
			if err != nil {
				errChatLog := &model_struct.LocalErrChatLog{}
				copier.Copy(errChatLog, v)
				exceptionMsg = append(exceptionMsg, errChatLog)
				log.ZWarn(ctx, "InsertMessage operation", err, "chatErrLog", errChatLog, "chatlog", v)
			}
		}
	}
	if err := c.db.BatchInsertExceptionMsgController(ctx, exceptionMsg); err != nil {
		log.ZError(ctx, "insert err message err", err, "msgs", exceptionMsg)
	}
	hList, _ := c.db.GetHiddenConversationList(ctx)
	for _, v := range hList {
		if nc, ok := newConversationSet[v.ConversationID]; ok {
			phConversationChangedSet[v.ConversationID] = nc
			nc.RecvMsgOpt = v.RecvMsgOpt
			nc.GroupAtType = v.GroupAtType
			nc.IsPinned = v.IsPinned
			nc.IsPrivateChat = v.IsPrivateChat
			if nc.IsPrivateChat {
				nc.BurnDuration = v.BurnDuration
			}
			nc.IsNotInGroup = v.IsNotInGroup
			nc.AttachedInfo = v.AttachedInfo
			nc.Ex = v.Ex
		}
	}

	for k, v := range newConversationSet {
		if _, ok := phConversationChangedSet[v.ConversationID]; !ok {
			phNewConversationSet[k] = v
		}
	}
	//Changed conversation storage

	if err := c.db.BatchUpdateConversationList(ctx, append(mapConversationToList(conversationChangedSet), mapConversationToList(phConversationChangedSet)...)); err != nil {
		log.ZError(ctx, "insert changed conversation err :", err)
	}
	//New conversation storage
	if err := c.db.BatchInsertConversationList(ctx, mapConversationToList(phNewConversationSet)); err != nil {
		log.ZError(ctx, "insert new conversation err:", err)
	}

	if err := c.db.BatchInsertConversationUnreadMessageList(ctx, unreadMessages); err != nil {
		log.ZError(ctx, "insert BatchInsertConversationUnreadMessageList err:", err)
	}
	c.doMsgReadState(ctx, msgReadList)

	c.DoGroupMsgReadState(ctx, groupMsgReadList)

	c.revokeMessage(ctx, msgRevokeList)
	if c.batchMsgListener != nil {
		c.batchNewMessages(ctx, newMessages)
	} else {
		c.newMessage(newMessages)
	}
	c.newRevokeMessage(ctx, newMsgRevokeList)
	c.doReactionMsgModifier(ctx, reactionMsgModifierList)
	c.doReactionMsgDeleter(ctx, reactionMsgDeleterList)
	//log.Info(operationID, "trigger map is :", newConversationSet, conversationChangedSet)
	if len(newConversationSet) > 0 {
		c.doUpdateConversation(common.Cmd2Value{Value: common.UpdateConNode{"", constant.NewConDirect, utils.StructToJsonString(mapConversationToList(newConversationSet))}})
	}
	if len(conversationChangedSet) > 0 {
		c.doUpdateConversation(common.Cmd2Value{Value: common.UpdateConNode{"", constant.ConChangeDirect, utils.StructToJsonString(mapConversationToList(conversationChangedSet))}})
	}
	if isTriggerUnReadCount {
		c.doUpdateConversation(common.Cmd2Value{Value: common.UpdateConNode{"", constant.TotalUnreadMessageChanged, ""}})
	}
}

func listToMap(list []*model_struct.LocalConversation, m map[string]*model_struct.LocalConversation) {
	for _, v := range list {
		m[v.ConversationID] = v
	}

}
func removeElementInList(a sdk_struct.NewMsgList, e *sdk_struct.MsgStruct) (b sdk_struct.NewMsgList) {
	for i := 0; i < len(a); i++ {
		if a[i] != e {
			b = append(b, a[i])
		}
	}
	return b
}
func (c *Conversation) diff(ctx context.Context, local, generated, cc, nc map[string]*model_struct.LocalConversation) {
	for _, v := range generated {
		log.Debug("node diff", *v)
		if localC, ok := local[v.ConversationID]; ok {

			if v.LatestMsgSendTime > localC.LatestMsgSendTime {
				localC.UnreadCount = localC.UnreadCount + v.UnreadCount
				localC.LatestMsg = v.LatestMsg
				localC.LatestMsgSendTime = v.LatestMsgSendTime
				cc[v.ConversationID] = localC
				log.Debug("", "diff1 ", *localC, *v)
			} else {
				localC.UnreadCount = localC.UnreadCount + v.UnreadCount
				cc[v.ConversationID] = localC
				log.Debug("", "diff2 ", *localC, *v)
			}

		} else {
			c.addFaceURLAndName(ctx, v)
			nc[v.ConversationID] = v
			log.Debug("", "diff3 ", *v)
		}
	}

}
func (c *Conversation) genConversationGroupAtType(lc *model_struct.LocalConversation, s *sdk_struct.MsgStruct) {
	if s.ContentType == constant.AtText {
		tagMe := utils.IsContain(c.loginUserID, s.AtElem.AtUserList)
		tagAll := utils.IsContain(constant.AtAllString, s.AtElem.AtUserList)
		if tagAll {
			if tagMe {
				lc.GroupAtType = constant.AtAllAtMe
				return
			}
			lc.GroupAtType = constant.AtAll
			return
		}
		if tagMe {
			lc.GroupAtType = constant.AtMe
		}
	}

}
func (c *Conversation) msgStructToLocalChatLog(m *sdk_struct.MsgStruct) *model_struct.LocalChatLog {
	var lc model_struct.LocalChatLog
	copier.Copy(&lc, m)
	if m.SessionType == constant.GroupChatType || m.SessionType == constant.SuperGroupChatType {
		lc.RecvID = m.GroupID
	}
	return &lc
}
func (c *Conversation) msgStructToLocalErrChatLog(m *sdk_struct.MsgStruct) *model_struct.LocalErrChatLog {
	var lc model_struct.LocalErrChatLog
	copier.Copy(&lc, m)
	if m.SessionType == constant.GroupChatType || m.SessionType == constant.SuperGroupChatType {
		lc.RecvID = m.GroupID
	}
	return &lc
}

// deprecated
func (c *Conversation) revokeMessage(ctx context.Context, msgRevokeList []*sdk_struct.MsgStruct) {
	for _, w := range msgRevokeList {
		if c.msgListener != nil {
			t := new(model_struct.LocalChatLog)
			t.ClientMsgID = w.Content
			t.Status = constant.MsgStatusRevoked
			t.SessionType = w.SessionType
			t.RecvID = w.GroupID
			err := c.db.UpdateMessageController(ctx, t)
			if err != nil {
				log.Error("internal", "setLocalMessageStatus revokeMessage err:", err.Error(), "msg", w)
			} else {
				log.Info("internal", "v.OnRecvMessageRevoked client_msg_id:", w.Content)
				c.msgListener.OnRecvMessageRevoked(w.Content)
			}
		} else {
			log.Error("internal", "set msgListener is err:")
		}
	}

}

func (c *Conversation) tempCacheChatLog(ctx context.Context, messageList []*sdk_struct.MsgStruct) {
	var newMessageList []*model_struct.TempCacheLocalChatLog
	copier.Copy(&newMessageList, &messageList)
	if err := c.db.BatchInsertTempCacheMessageList(ctx, newMessageList); err != nil {
		log.Error("", "BatchInsertTempCacheMessageList detail err:", err.Error(), len(newMessageList))
		for _, v := range newMessageList {
			err := c.db.InsertTempCacheMessage(ctx, v)
			if err != nil {
				log.ZWarn(ctx, "InsertTempCacheMessage operation", err, "chat err log: ", *v)
			}
		}
	}
}
func (c *Conversation) newRevokeMessage(ctx context.Context, msgRevokeList []*sdk_struct.MsgStruct) {
	var failedRevokeMessageList []*sdk_struct.MsgStruct
	var superGroupIDList []string
	var revokeMessageRevoked []*sdk_struct.MessageRevoked
	var superGroupRevokeMessageRevoked []*sdk_struct.MessageRevoked
	log.NewDebug("revoke msg", msgRevokeList)
	for _, w := range msgRevokeList {
		log.NewDebug("msg revoke", w)
		var msg sdk_struct.MessageRevoked
		err := json.Unmarshal([]byte(w.Content), &msg)
		if err != nil {
			log.Error("internal", "unmarshal failed, err : ", err.Error(), *w)
			continue
		}
		t := new(model_struct.LocalChatLog)
		t.ClientMsgID = msg.ClientMsgID
		t.Status = constant.MsgStatusRevoked
		t.SessionType = msg.SessionType
		t.RecvID = w.GroupID
		err = c.db.UpdateMessageController(ctx, t)
		if err != nil {
			log.Error("internal", "setLocalMessageStatus revokeMessage err:", err.Error(), "msg", w)
			failedRevokeMessageList = append(failedRevokeMessageList, w)
			switch msg.SessionType {
			case constant.SuperGroupChatType:
				err := c.db.InsertMessageController(ctx, t)
				if err != nil {
					log.Error("internal", "InsertMessageController preDefine Message err:", err.Error(), "msg", *t)
				}
			}
		} else {
			t := new(model_struct.LocalChatLog)
			t.ClientMsgID = w.ClientMsgID
			t.SendTime = msg.SourceMessageSendTime
			t.SessionType = w.SessionType
			t.RecvID = w.GroupID
			err = c.db.UpdateMessageController(ctx, t)
			if err != nil {
				log.Error("internal", "setLocalMessageStatus revokeMessage err:", err.Error(), "msg", w)
			}
			log.Info("internal", "v.OnNewRecvMessageRevoked client_msg_id:", msg.ClientMsgID)
			if c.msgListener != nil {
				c.msgListener.OnNewRecvMessageRevoked(w.Content)
			} else {
				log.Error("internal", "set msgListener is err:")
			}
			if msg.SessionType != constant.SuperGroupChatType {
				revokeMessageRevoked = append(revokeMessageRevoked, &msg)
			} else {
				if !utils.IsContain(w.RecvID, superGroupIDList) {
					superGroupIDList = append(superGroupIDList, w.GroupID)
				}
				superGroupRevokeMessageRevoked = append(superGroupRevokeMessageRevoked, &msg)
			}
		}
	}
	log.NewDebug("internal, quoteRevoke Info", superGroupIDList, len(revokeMessageRevoked), len(superGroupRevokeMessageRevoked))
	if len(revokeMessageRevoked) > 0 {
		msgList, err := c.db.SearchAllMessageByContentType(ctx, constant.Quote)
		if err != nil {
			log.NewError("internal", "SearchMessageIDsByContentType failed", err.Error())
		}
		for _, v := range msgList {
			c.QuoteMsgRevokeHandle(ctx, v, revokeMessageRevoked)
		}
	}
	for _, superGroupID := range superGroupIDList {
		msgList, err := c.db.SuperGroupSearchAllMessageByContentType(ctx, superGroupID, constant.Quote)
		if err != nil {
			log.NewError("internal", "SuperGroupSearchMessageByContentTypeNotOffset failed", superGroupID, err.Error())
		}
		for _, v := range msgList {
			c.QuoteMsgRevokeHandle(ctx, v, superGroupRevokeMessageRevoked)
		}
	}
	if len(failedRevokeMessageList) > 0 {
		//c.tempCacheChatLog(failedRevokeMessageList)
	}
}
func (c *Conversation) DoMsgReaction(msgReactionList []*sdk_struct.MsgStruct) {

	//for _, v := range msgReactionList {
	//	var msg sdk_struct.MessageReaction
	//	err := json.Unmarshal([]byte(v.Content), &msg)
	//	if err != nil {
	//		log.Error("internal", "unmarshal failed, err : ", err.Error(), *v)
	//		continue
	//	}
	//	s := sdk_struct.MsgStruct{GroupID: msg.GroupID, ClientMsgID: msg.ClientMsgID, SessionType: msg.SessionType}
	//	message, err := c.db.GetMessageController(&s)
	//	if err != nil {
	//		log.Error("internal", "GetMessageController, err : ", err.Error(), s)
	//		continue
	//	}
	//	t := new(model_struct.LocalChatLog)
	//  attachInfo := sdk_struct.AttachedInfoElem{}
	//	_ = utils.JsonStringToStruct(message.AttachedInfo, &attachInfo)
	//
	//	contain, v := isContainMessageReaction(msg.ReactionType, attachInfo.MessageReactionElem)
	//	if contain {
	//		userContain, userReaction := isContainUserReactionElem(msg.UserID, v.UserReactionList)
	//		if userContain {
	//			if !v.CanRepeat && userReaction.Counter > 0 {
	//				// to do nothing
	//				continue
	//			} else {
	//				userReaction.Counter += msg.Counter
	//				v.Counter += msg.Counter
	//				if v.Counter < 0 {
	//					log.Debug("internal", "after operate all counter  < 0", v.Type, v.Counter, msg.Counter)
	//					v.Counter = 0
	//				}
	//				if userReaction.Counter <= 0 {
	//					log.Debug("internal", "after operate userReaction counter < 0", v.Type, userReaction.Counter, msg.Counter)
	//					v.UserReactionList = DeleteUserReactionElem(v.UserReactionList, c.loginUserID)
	//				}
	//			}
	//		} else {
	//			log.Debug("internal", "attachInfo.MessageReactionElem is nil", msg)
	//			u := new(sdk_struct.UserReactionElem)
	//			u.UserID = msg.UserID
	//			u.Counter = msg.Counter
	//			v.Counter += msg.Counter
	//			if v.Counter < 0 {
	//				log.Debug("internal", "after operate all counter  < 0", v.Type, v.Counter, msg.Counter)
	//				v.Counter = 0
	//			}
	//			if u.Counter <= 0 {
	//				log.Debug("internal", "after operate userReaction counter < 0", v.Type, u.Counter, msg.Counter)
	//				v.UserReactionList = DeleteUserReactionElem(v.UserReactionList, msg.UserID)
	//			}
	//			v.UserReactionList = append(v.UserReactionList, u)
	//
	//		}
	//
	//	} else {
	//		log.Debug("internal", "attachInfo.MessageReactionElem is nil", msg)
	//		t := new(sdk_struct.ReactionElem)
	//		t.Counter = msg.Counter
	//		t.Type = msg.ReactionType
	//		u := new(sdk_struct.UserReactionElem)
	//		u.UserID = msg.UserID
	//		u.Counter = msg.Counter
	//		t.UserReactionList = append(t.UserReactionList, u)
	//		attachInfo.MessageReactionElem = append(attachInfo.MessageReactionElem, t)
	//
	//	}
	//
	//	t.AttachedInfo = utils.StructToJsonString(attachInfo)
	//	t.ClientMsgID = message.ClientMsgID
	//
	//	t.SessionType = message.SessionType
	//	t.RecvID = message.RecvID
	//	err1 := c.db.UpdateMessageController(t)
	//	if err1 != nil {
	//		log.Error("internal", "UpdateMessageController err:", err1, "ClientMsgID", *t, message)
	//	}
	//	c.doUpdateConversation(common.Cmd2Value{Value: common.UpdateConNode{"", constant.MessageChange, &s}})
	//
	//}
}

func (c *Conversation) doReactionMsgModifier(ctx context.Context, msgReactionList []*sdk_struct.MsgStruct) {
	for _, msgStruct := range msgReactionList {
		var n server_api_params.ReactionMessageModifierNotification
		err := json.Unmarshal([]byte(msgStruct.Content), &n)
		if err != nil {
			log.Error("internal", "unmarshal failed err:", err.Error(), *msgStruct)
			continue
		}
		switch n.Operation {
		case constant.AddMessageExtensions:
			var reactionExtensionList []*sdkws.KeyValue
			for _, value := range n.SuccessReactionExtensionList {
				reactionExtensionList = append(reactionExtensionList, value)
			}
			if !(msgStruct.SendID == c.loginUserID && msgStruct.SenderPlatformID == c.platformID) {
				c.msgListener.OnRecvMessageExtensionsAdded(n.ClientMsgID, utils.StructToJsonString(reactionExtensionList))
			}
		case constant.SetMessageExtensions:
			err = c.db.GetAndUpdateMessageReactionExtension(ctx, n.ClientMsgID, n.SuccessReactionExtensionList)
			if err != nil {
				log.Error("internal", "GetAndUpdateMessageReactionExtension err:", err.Error())
				continue
			}
			var reactionExtensionList []*sdkws.KeyValue
			for _, value := range n.SuccessReactionExtensionList {
				reactionExtensionList = append(reactionExtensionList, value)
			}
			if !(msgStruct.SendID == c.loginUserID && msgStruct.SenderPlatformID == c.platformID) {
				c.msgListener.OnRecvMessageExtensionsChanged(n.ClientMsgID, utils.StructToJsonString(reactionExtensionList))
			}

		}
		t := model_struct.LocalChatLog{}
		t.ClientMsgID = n.ClientMsgID
		t.SessionType = n.SessionType
		t.IsExternalExtensions = n.IsExternalExtensions
		t.IsReact = n.IsReact
		t.MsgFirstModifyTime = n.MsgFirstModifyTime
		if n.SessionType == constant.GroupChatType || n.SessionType == constant.SuperGroupChatType {
			t.RecvID = n.SourceID
		}
		err2 := c.db.UpdateMessageController(ctx, &t)
		if err2 != nil {
			log.Error("internal", "unmarshal failed err:", err2.Error(), t)
			continue
		}

	}

}
func (c *Conversation) doReactionMsgDeleter(ctx context.Context, msgReactionList []*sdk_struct.MsgStruct) {
	for _, msgStruct := range msgReactionList {
		var n server_api_params.ReactionMessageDeleteNotification
		err := json.Unmarshal([]byte(msgStruct.Content), &n)
		if err != nil {
			log.Error("internal", "unmarshal failed err:", err.Error(), *msgStruct)
			continue
		}
		err = c.db.DeleteAndUpdateMessageReactionExtension(ctx, n.ClientMsgID, n.SuccessReactionExtensionList)
		if err != nil {
			log.Error("internal", "GetAndUpdateMessageReactionExtension err:", err.Error())
			continue
		}
		var deleteKeyList []string
		for _, value := range n.SuccessReactionExtensionList {
			deleteKeyList = append(deleteKeyList, value.TypeKey)
		}
		c.msgListener.OnRecvMessageExtensionsDeleted(n.ClientMsgID, utils.StructToJsonString(deleteKeyList))

	}

}
func (c *Conversation) QuoteMsgRevokeHandle(ctx context.Context, v *model_struct.LocalChatLog, revokeMsgIDList []*sdk_struct.MessageRevoked) {
	s := sdk_struct.MsgStruct{}
	err := utils.JsonStringToStruct(v.Content, &s.QuoteElem)
	if err != nil {
		log.NewError("internal", "unmarshall failed", s.Content)
	}
	if s.QuoteElem.QuoteMessage == nil {
		return
	}
	ok, revokeMessage := isContainRevokedList(s.QuoteElem.QuoteMessage.ClientMsgID, revokeMsgIDList)
	if !ok {
		return
	}
	s.QuoteElem.QuoteMessage.Content = utils.StructToJsonString(revokeMessage)
	s.QuoteElem.QuoteMessage.ContentType = constant.AdvancedRevoke
	v.Content = utils.StructToJsonString(s.QuoteElem)
	err = c.db.UpdateMessageController(ctx, v)
	if err != nil {
		log.NewError("internal", "unmarshall failed", v)
	}
}
func isContainRevokedList(target string, List []*sdk_struct.MessageRevoked) (bool, *sdk_struct.MessageRevoked) {
	for _, element := range List {
		if target == element.ClientMsgID {
			return true, element
		}
	}
	return false, nil
}

func (c *Conversation) DoGroupMsgReadState(ctx context.Context, groupMsgReadList []*sdk_struct.MsgStruct) {
	var groupMessageReceiptResp []*sdk_struct.MessageReceipt
	var failedMessageList []*sdk_struct.MsgStruct
	userMsgMap := make(map[string]map[string][]string)
	//var temp []*sdk_struct.MessageReceipt
	for _, rd := range groupMsgReadList {
		var list []string
		err := json.Unmarshal([]byte(rd.Content), &list)
		if err != nil {
			log.Error("internal", "unmarshal failed, err : ", err.Error(), rd)
			continue
		}
		if groupMap, ok := userMsgMap[rd.SendID]; ok {
			if oldMsgIDList, ok := groupMap[rd.GroupID]; ok {
				oldMsgIDList = append(oldMsgIDList, list...)
				groupMap[rd.GroupID] = oldMsgIDList
			} else {
				groupMap[rd.GroupID] = list
			}
		} else {
			g := make(map[string][]string)
			g[rd.GroupID] = list
			userMsgMap[rd.SendID] = g
		}

	}
	for userID, m := range userMsgMap {
		for groupID, msgIDList := range m {
			var successMsgIDlist []string
			var failedMsgIDList []string
			newMsgID := utils.RemoveRepeatedStringInList(msgIDList)
			_, sessionType, err := c.getConversationTypeByGroupID(ctx, groupID)
			if err != nil {
				log.Error("internal", "GetGroupInfoByGroupID err:", err.Error(), "groupID", groupID)
				continue
			}
			messages, err := c.db.GetMultipleMessageController(ctx, newMsgID, groupID, sessionType)
			if err != nil {
				log.Error("internal", "GetMessage err:", err.Error(), "ClientMsgID", newMsgID)
				continue
			}
			msgRt := new(sdk_struct.MessageReceipt)
			msgRt.UserID = userID
			msgRt.GroupID = groupID
			msgRt.SessionType = sessionType
			msgRt.ContentType = constant.GroupHasReadReceipt

			for _, message := range messages {
				t := new(model_struct.LocalChatLog)
				if userID != c.loginUserID {
					attachInfo := sdk_struct.AttachedInfoElem{}
					_ = utils.JsonStringToStruct(message.AttachedInfo, &attachInfo)
					attachInfo.GroupHasReadInfo.HasReadUserIDList = utils.RemoveRepeatedStringInList(append(attachInfo.GroupHasReadInfo.HasReadUserIDList, userID))
					attachInfo.GroupHasReadInfo.HasReadCount = int32(len(attachInfo.GroupHasReadInfo.HasReadUserIDList))
					t.AttachedInfo = utils.StructToJsonString(attachInfo)
				}
				t.ClientMsgID = message.ClientMsgID
				t.IsRead = true
				t.SessionType = message.SessionType
				t.RecvID = message.RecvID
				if err := c.db.UpdateMessageController(ctx, t); err != nil {
					log.Error("internal", "setGroupMessageHasReadByMsgID err:", err, "ClientMsgID", t, message)
					continue
				}
				successMsgIDlist = append(successMsgIDlist, message.ClientMsgID)
			}
			failedMsgIDList = utils.DifferenceSubsetString(newMsgID, successMsgIDlist)
			if len(successMsgIDlist) != 0 {
				msgRt.MsgIDList = successMsgIDlist
				groupMessageReceiptResp = append(groupMessageReceiptResp, msgRt)
			}
			if len(failedMsgIDList) != 0 {
				m := new(sdk_struct.MsgStruct)
				m.ClientMsgID = utils.GetMsgID(userID)
				m.SendID = userID
				m.GroupID = groupID
				m.ContentType = constant.GroupHasReadReceipt
				m.Content = utils.StructToJsonString(failedMsgIDList)
				m.Status = constant.MsgStatusFiltered
				failedMessageList = append(failedMessageList, m)
			}
		}
	}
	if len(groupMessageReceiptResp) > 0 {
		log.Info("internal", "OnRecvGroupReadReceipt: ", utils.StructToJsonString(groupMessageReceiptResp))
		c.msgListener.OnRecvGroupReadReceipt(utils.StructToJsonString(groupMessageReceiptResp))
	}
	if len(failedMessageList) > 0 {
		//c.tempCacheChatLog(failedMessageList)
	}
}
func (c *Conversation) newMessage(newMessagesList sdk_struct.NewMsgList) {
	sort.Sort(newMessagesList)
	for _, w := range newMessagesList {
		log.Info("internal", "newMessage: ", w.ClientMsgID)
		if c.msgListener != nil {
			log.Info("internal", "msgListener,OnRecvNewMessage")
			c.msgListener.OnRecvNewMessage(utils.StructToJsonString(w))
		} else {
			log.Error("internal", "set msgListener is err ")
		}
		if c.listenerForService != nil {
			log.Info("internal", "msgListener,OnRecvNewMessage")
			c.listenerForService.OnRecvNewMessage(utils.StructToJsonString(w))
		}
	}
}
func (c *Conversation) batchNewMessages(ctx context.Context, newMessagesList sdk_struct.NewMsgList) {
	sort.Sort(newMessagesList)
	if c.batchMsgListener != nil {
		if len(newMessagesList) > 0 {
			c.batchMsgListener.OnRecvNewMessages(utils.StructToJsonString(newMessagesList))
			//if c.IsBackground {
			//	c.batchMsgListener.OnRecvOfflineNewMessages(utils.StructToJsonString(newMessagesList))
			//}
		}
	} else {
		log.ZWarn(ctx, "not set batchMsgListener", nil)
	}

}

func (c *Conversation) doMsgReadState(ctx context.Context, msgReadList []*sdk_struct.MsgStruct) {
	var messageReceiptResp []*sdk_struct.MessageReceipt
	var msgIdList []string
	chrsList := make(map[string][]string)
	var conversationID string

	for _, rd := range msgReadList {
		err := json.Unmarshal([]byte(rd.Content), &msgIdList)
		if err != nil {
			log.Error("internal", "unmarshal failed, err : ", err.Error())
			return
		}
		var msgIdListStatusOK []string
		for _, v := range msgIdList {
			m, err := c.db.GetMessage(ctx, v)
			if err != nil {
				log.Error("internal", "GetMessage err:", err, "ClientMsgID", v)
				continue
			}
			attachInfo := sdk_struct.AttachedInfoElem{}
			_ = utils.JsonStringToStruct(m.AttachedInfo, &attachInfo)
			attachInfo.HasReadTime = rd.SendTime
			m.AttachedInfo = utils.StructToJsonString(attachInfo)
			m.IsRead = true
			err = c.db.UpdateMessage(ctx, m)
			if err != nil {
				log.Error("internal", "setMessageHasReadByMsgID err:", err, "ClientMsgID", v)
				continue
			}

			msgIdListStatusOK = append(msgIdListStatusOK, v)
		}
		if len(msgIdListStatusOK) > 0 {
			msgRt := new(sdk_struct.MessageReceipt)
			msgRt.ContentType = rd.ContentType
			msgRt.MsgFrom = rd.MsgFrom
			msgRt.ReadTime = rd.SendTime
			msgRt.UserID = rd.SendID
			msgRt.SessionType = constant.SingleChatType
			msgRt.MsgIDList = msgIdListStatusOK
			messageReceiptResp = append(messageReceiptResp, msgRt)
		}
		if rd.SendID == c.loginUserID {
			conversationID = utils.GetConversationIDBySessionType(rd.RecvID, constant.SingleChatType)
		} else {
			conversationID = utils.GetConversationIDBySessionType(rd.SendID, constant.SingleChatType)
		}
		if v, ok := chrsList[conversationID]; ok {
			chrsList[conversationID] = append(v, msgIdListStatusOK...)
		} else {
			chrsList[conversationID] = msgIdListStatusOK
		}
		c.doUpdateConversation(common.Cmd2Value{Value: common.UpdateConNode{Action: constant.ConversationLatestMsgHasRead, Args: chrsList}})
	}
	if len(messageReceiptResp) > 0 {

		log.Info("internal", "OnRecvC2CReadReceipt: ", utils.StructToJsonString(messageReceiptResp))
		c.msgListener.OnRecvC2CReadReceipt(utils.StructToJsonString(messageReceiptResp))
	}
}

type messageKvList struct {
	ClientMsgID   string                      `json:"clientMsgID"`
	ChangedKvList []*sdk.SingleTypeKeyInfoSum `json:"changedKvList"`
}

func (c *Conversation) msgConvert(msg *sdk_struct.MsgStruct) (err error) {
	err = c.msgHandleByContentType(msg)
	if err != nil {
		return err
	} else {
		if msg.SessionType == constant.GroupChatType {
			msg.GroupID = msg.RecvID
			msg.RecvID = c.loginUserID
		}
		return nil
	}
}

func (c *Conversation) msgHandleByContentType(msg *sdk_struct.MsgStruct) (err error) {
	_ = utils.JsonStringToStruct(msg.AttachedInfo, &msg.AttachedInfoElem)
	if msg.ContentType >= constant.NotificationBegin && msg.ContentType <= constant.NotificationEnd {
		var tips sdkws.TipsComm
		err = utils.JsonStringToStruct(msg.Content, &tips)
		msg.NotificationElem.Detail = tips.JsonDetail
		msg.NotificationElem.DefaultTips = tips.DefaultTips
	} else {
		switch msg.ContentType {
		case constant.Text:
			if msg.AttachedInfoElem.IsEncryption && c.encryptionKey != "" && msg.AttachedInfoElem.InEncryptStatus {
				var newContent []byte
				log.NewDebug("", utils.GetSelfFuncName(), "org content, key", msg.Content, c.encryptionKey, []byte(msg.Content), msg.CreateTime, msg.AttachedInfoElem, msg.AttachedInfo)
				newContent, err = utils.AesDecrypt([]byte(msg.Content), []byte(c.encryptionKey))
				msg.Content = string(newContent)
				msg.AttachedInfoElem.InEncryptStatus = false
				msg.AttachedInfo = utils.StructToJsonString(msg.AttachedInfoElem)
			}
		case constant.Picture:
			err = utils.JsonStringToStruct(msg.Content, &msg.PictureElem)
		case constant.Voice:
			err = utils.JsonStringToStruct(msg.Content, &msg.SoundElem)
		case constant.Video:
			err = utils.JsonStringToStruct(msg.Content, &msg.VideoElem)
		case constant.File:
			err = utils.JsonStringToStruct(msg.Content, &msg.FileElem)
		case constant.AdvancedText:
			err = utils.JsonStringToStruct(msg.Content, &msg.MessageEntityElem)
		case constant.AtText:
			err = utils.JsonStringToStruct(msg.Content, &msg.AtElem)
			if err == nil {
				if utils.IsContain(c.loginUserID, msg.AtElem.AtUserList) {
					msg.AtElem.IsAtSelf = true
				}
			}
		case constant.Location:
			err = utils.JsonStringToStruct(msg.Content, &msg.LocationElem)
		case constant.Custom:
			err = utils.JsonStringToStruct(msg.Content, &msg.CustomElem)
		case constant.Quote:
			err = utils.JsonStringToStruct(msg.Content, &msg.QuoteElem)
		case constant.Merger:
			err = utils.JsonStringToStruct(msg.Content, &msg.MergeElem)
		case constant.Face:
			err = utils.JsonStringToStruct(msg.Content, &msg.FaceElem)
		case constant.CustomMsgNotTriggerConversation:
			err = utils.JsonStringToStruct(msg.Content, &msg.CustomElem)
		case constant.CustomMsgOnlineOnly:
			err = utils.JsonStringToStruct(msg.Content, &msg.CustomElem)
		}
	}

	return utils.Wrap(err, "")
}
func (c *Conversation) updateConversation(lc *model_struct.LocalConversation, cs map[string]*model_struct.LocalConversation) {
	if oldC, ok := cs[lc.ConversationID]; !ok {
		cs[lc.ConversationID] = lc
	} else {
		if lc.LatestMsgSendTime > oldC.LatestMsgSendTime {
			oldC.UnreadCount = oldC.UnreadCount + lc.UnreadCount
			oldC.LatestMsg = lc.LatestMsg
			oldC.LatestMsgSendTime = lc.LatestMsgSendTime
			cs[lc.ConversationID] = oldC
		} else {
			oldC.UnreadCount = oldC.UnreadCount + lc.UnreadCount
			cs[lc.ConversationID] = oldC
		}
	}
	//if oldC, ok := cc[lc.ConversationID]; !ok {
	//	oc, err := c.db.GetConversation(lc.ConversationID)
	//	if err == nil && oc.ConversationID != "" {//如果会话已经存在
	//		if lc.LatestMsgSendTime > oc.LatestMsgSendTime {
	//			oc.UnreadCount = oc.UnreadCount + lc.UnreadCount
	//			oc.LatestMsg = lc.LatestMsg
	//			oc.LatestMsgSendTime = lc.LatestMsgSendTime
	//			cc[lc.ConversationID] = *oc
	//		} else {
	//			oc.UnreadCount = oc.UnreadCount + lc.UnreadCount
	//			cc[lc.ConversationID] = *oc
	//		}
	//	} else {
	//		if oldC, ok := nc[lc.ConversationID]; !ok {
	//			c.addFaceURLAndName(lc)
	//			nc[lc.ConversationID] = *lc
	//		} else {
	//			if lc.LatestMsgSendTime > oldC.LatestMsgSendTime {
	//				oldC.UnreadCount = oldC.UnreadCount + lc.UnreadCount
	//				oldC.LatestMsg = lc.LatestMsg
	//				oldC.LatestMsgSendTime = lc.LatestMsgSendTime
	//				nc[lc.ConversationID] = oldC
	//			} else {
	//				oldC.UnreadCount = oldC.UnreadCount + lc.UnreadCount
	//				nc[lc.ConversationID] = oldC
	//			}
	//		}
	//	}
	//} else {
	//	if lc.LatestMsgSendTime > oldC.LatestMsgSendTime {
	//		oldC.UnreadCount = oldC.UnreadCount + lc.UnreadCount
	//		oldC.LatestMsg = lc.LatestMsg
	//		oldC.LatestMsgSendTime = lc.LatestMsgSendTime
	//		cc[lc.ConversationID] = oldC
	//	} else {
	//		oldC.UnreadCount = oldC.UnreadCount + lc.UnreadCount
	//		cc[lc.ConversationID] = oldC
	//	}
	//
	//}

}
func mapConversationToList(m map[string]*model_struct.LocalConversation) (cs []*model_struct.LocalConversation) {
	for _, v := range m {
		cs = append(cs, v)
	}
	return cs
}
func (c *Conversation) addFaceURLAndName(ctx context.Context, lc *model_struct.LocalConversation) error {
	switch lc.ConversationType {
	case constant.SingleChatType, constant.NotificationChatType:
		faceUrl, name, err := c.cache.GetUserNameAndFaceURL(ctx, lc.UserID)
		if err != nil {
			return err
		}
		lc.FaceURL = faceUrl
		lc.ShowName = name

	case constant.GroupChatType, constant.SuperGroupChatType:
		g, err := c.full.GetGroupInfoFromLocal2Svr(ctx, lc.GroupID, lc.ConversationType)
		if err != nil {
			return err
		}
		lc.ShowName = g.GroupName
		lc.FaceURL = g.FaceURL
	}
	return nil
}
