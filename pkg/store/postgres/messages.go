package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/getzep/zep/internal"
	"github.com/google/uuid"

	"github.com/getzep/zep/pkg/models"
	"github.com/getzep/zep/pkg/store"
	"github.com/jinzhu/copier"
	"github.com/uptrace/bun"
)

// putMessages stores a new or updates existing messages for a session. Existing
// messages are determined by message UUID. Sessions are created if they do not
// exist.
// If the session is deleted, an error is returned.
func putMessages(
	ctx context.Context,
	db *bun.DB,
	sessionID string,
	messages []models.Message,
) ([]models.Message, error) {
	if len(messages) == 0 {
		log.Warn("putMessages called with no messages")
		return nil, nil
	}
	log.Debugf(
		"putMessages called for session %s with %d messages",
		sessionID,
		len(messages),
	)

	// Try Update the session first. If no rows are affected, create a new session.
	sessionStore := NewSessionDAO(db)
	_, err := sessionStore.Update(ctx, &models.UpdateSessionRequest{
		SessionID: sessionID,
	}, false)
	if err != nil {
		if errors.Is(err, models.ErrNotFound) {
			_, err = sessionStore.Create(ctx, &models.CreateSessionRequest{
				SessionID: sessionID,
			})
			if err != nil {
				return nil, err
			}
		} else {
			return nil, err
		}
	}

	pgMessages := make([]MessageStoreSchema, len(messages))
	for i, msg := range messages {
		pgMessages[i] = MessageStoreSchema{
			UUID:       msg.UUID,
			SessionID:  sessionID,
			Role:       msg.Role,
			Content:    msg.Content,
			TokenCount: msg.TokenCount,
			Metadata:   msg.Metadata,
		}
	}

	// Insert messages
	_, err = db.NewInsert().
		Model(&pgMessages).
		Column("uuid", "session_id", "role", "content", "token_count", "updated_at").
		On("CONFLICT (uuid) DO UPDATE").
		Exec(ctx)
	if err != nil {
		return nil, store.NewStorageError("failed to Create messages", err)
	}

	// copy the UUIDs back into the original messages
	// this is needed if the messages are new and not being updated
	for i := range messages {
		messages[i].UUID = pgMessages[i].UUID
	}

	// insert/update message metadata. isPrivileged is false because we are
	// most likely being called by the PutMemory handler.
	messages, err = putMessageMetadata(ctx, db, sessionID, messages, false)
	if err != nil {
		return nil, err
	}

	log.Debugf("putMessages completed for session %s with %d messages", sessionID, len(messages))

	return messages, nil
}

// getMessageList retrieves all messages for a sessionID with pagination.
func getMessageList(
	ctx context.Context,
	db *bun.DB,
	sessionID string,
	currentPage int,
	pageSize int,
) (*models.MessageListResponse, error) {
	if sessionID == "" {
		return nil, store.NewStorageError("sessionID cannot be empty", nil)
	}
	if pageSize < 1 {
		return nil, store.NewStorageError("pageSize must be greater than 0", nil)
	}

	// Get count of all messages for this session
	count, err := db.NewSelect().
		Model(&MessageStoreSchema{}).
		Where("session_id = ?", sessionID).
		Count(ctx)
	if err != nil {
		return nil, store.NewStorageError("failed to get message count", err)
	}

	// Get messages
	var messages []MessageStoreSchema
	err = db.NewSelect().
		Model(&messages).
		Where("session_id = ?", sessionID).
		OrderExpr("id ASC").
		Limit(pageSize).
		Offset((currentPage - 1) * pageSize).
		Scan(ctx)
	if err != nil {
		return nil, store.NewStorageError("failed to get messages", err)
	}
	if len(messages) == 0 {
		return nil, nil
	}

	messageList := make([]models.Message, len(messages))
	for i, msg := range messages {
		messageList[i] = models.Message{
			UUID:       msg.UUID,
			CreatedAt:  msg.CreatedAt,
			Role:       msg.Role,
			Content:    msg.Content,
			TokenCount: msg.TokenCount,
			Metadata:   msg.Metadata,
		}
	}

	r := &models.MessageListResponse{
		Messages:   messageList,
		TotalCount: count,
		RowCount:   len(messages),
	}

	return r, nil
}

func getMessagesByUUID(
	ctx context.Context,
	db *bun.DB,
	sessionID string,
	uuids []uuid.UUID,
) ([]models.Message, error) {
	if sessionID == "" {
		return nil, errors.New("sessionID cannot be empty")
	}

	if len(uuids) == 0 {
		return nil, nil
	}

	var messages []MessageStoreSchema
	err := db.NewSelect().
		Model(&messages).
		Where("session_id = ?", sessionID).
		Where("uuid IN (?)", bun.In(uuids)).
		Scan(ctx)

	if err != nil {
		return nil, fmt.Errorf("unable to retrieve messages %w", err)
	}

	messageList := make([]models.Message, len(messages))
	for i, msg := range messages {
		messageList[i] = models.Message{
			UUID:       msg.UUID,
			CreatedAt:  msg.CreatedAt,
			Role:       msg.Role,
			Content:    msg.Content,
			TokenCount: msg.TokenCount,
			Metadata:   msg.Metadata,
		}
	}

	return messageList, nil
}

// getMessages retrieves recent messages from the memory store. If lastNMessages is 0, the last SummaryPoint is retrieved.
func getMessages(
	ctx context.Context,
	db *bun.DB,
	sessionID string,
	memoryWindow int,
	summary *models.Summary,
	lastNMessages int,
) ([]models.Message, error) {
	if sessionID == "" {
		return nil, store.NewStorageError("sessionID cannot be empty", nil)
	}
	if memoryWindow == 0 {
		return nil, store.NewStorageError("memory.message_window must be greater than 0", nil)
	}

	var messages []MessageStoreSchema
	var err error
	if lastNMessages > 0 {
		messages, err = fetchLastNMessages(ctx, db, sessionID, lastNMessages)
	} else {
		messages, err = fetchMessagesAfterSummaryPoint(ctx, db, sessionID, summary, memoryWindow)
	}
	if err != nil {
		return nil, store.NewStorageError("failed to get messages", err)
	}
	if len(messages) == 0 {
		return nil, nil
	}

	messageList := make([]models.Message, len(messages))
	err = copier.Copy(&messageList, &messages)
	if err != nil {
		return nil, store.NewStorageError("failed to copy messages", err)
	}

	return messageList, nil
}

// fetchMessagesAfterSummaryPoint retrieves messages after a summary point. If the summaryPointIndex
// is 0, all undeleted messages are retrieved.
func fetchMessagesAfterSummaryPoint(
	ctx context.Context,
	db *bun.DB,
	sessionID string,
	summary *models.Summary,
	memoryWindow int,
) ([]MessageStoreSchema, error) {
	var summaryPointIndex int64
	var err error
	if summary != nil {
		summaryPointIndex, err = getSummaryPointIndex(ctx, db, sessionID, summary.SummaryPointUUID)
		if err != nil {
			return nil, store.NewStorageError("unable to retrieve summary", nil)
		}
	}

	messages := make([]MessageStoreSchema, 0)
	query := db.NewSelect().
		Model(&messages).
		Where("session_id = ?", sessionID).
		Order("id ASC")

	if summaryPointIndex > 0 {
		query.Where("id > ?", summaryPointIndex)
	}

	// Always limit to the memory window
	query.Limit(memoryWindow)

	return messages, query.Scan(ctx)
}

// fetchLastNMessages retrieves the last N messages for a session, ordered by ID DESC
// and then reverses the slice so that the messages are in ascending order
func fetchLastNMessages(
	ctx context.Context,
	db *bun.DB,
	sessionID string,
	lastNMessages int,
) ([]MessageStoreSchema, error) {
	messages := make([]MessageStoreSchema, 0)
	query := db.NewSelect().
		Model(&messages).
		Where("session_id = ?", sessionID).
		Order("id DESC").
		Limit(lastNMessages)

	err := query.Scan(ctx)

	if err == nil && len(messages) > 0 {
		internal.ReverseSlice(messages)
	}

	return messages, err
}

// getSummaryPointIndex retrieves the index of the last summary point for a session
// This is a bit of a hack since UUIDs are not sortable.
// If the SummaryPoint does not exist (for e.g. if it was deleted), returns 0.
func getSummaryPointIndex(
	ctx context.Context,
	db *bun.DB,
	sessionID string,
	summaryPointUUID uuid.UUID,
) (int64, error) {
	var message MessageStoreSchema

	err := db.NewSelect().
		Model(&message).
		Column("id").
		Where("session_id = ? AND uuid = ?", sessionID, summaryPointUUID).
		Scan(ctx)

	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			log.Warningf(
				"unable to retrieve last summary point for %s: %s",
				summaryPointUUID,
				err,
			)
		} else {
			return 0, store.NewStorageError("unable to retrieve last summary point for %s", err)
		}

		return 0, nil
	}

	return message.ID, nil
}
