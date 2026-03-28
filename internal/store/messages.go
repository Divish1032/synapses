package store

import (
	"database/sql"
	"fmt"
	"time"
)

// Message is a single entry in the agent message bus.
// Agents send messages to specific peers (to_agent non-empty) or broadcast
// to all agents (to_agent empty). The seq field acts as a cursor for polling.
type Message struct {
	Seq       int64  `json:"seq"`
	ID        string `json:"id"`
	FromAgent string `json:"from_agent"`
	ToAgent   string `json:"to_agent,omitempty"` // empty = broadcast
	Topic     string `json:"topic"`
	Payload   string `json:"payload"` // arbitrary JSON
	ProjectID string `json:"project_id,omitempty"`
	CreatedAt int64  `json:"created_at"`        // Unix seconds
	ReadAt    *int64 `json:"read_at,omitempty"` // nil = unread
}

// SendMessage inserts a new message into the bus and returns the generated
// message ID. toAgent may be empty to broadcast to all agents.
// payload should be a valid JSON string (e.g. "{}").
// messageTTL defines retention windows for the agent_messages table.
// Read messages are cheap to discard early; unread messages are kept longer
// so agents that are temporarily offline don't miss broadcasts.
const (
	msgReadTTL   = 24 * time.Hour     // prune already-read messages after 24 h
	msgUnreadTTL = 7 * 24 * time.Hour // prune unread messages after 7 days
)

// SendMessage stores a new inter-agent message and returns its ID.
func (s *Store) SendMessage(fromAgent, toAgent, topic, payload, projectID string) (string, error) {
	id := newID()
	now := time.Now()
	nowUnix := now.Unix()

	// Normalise: store empty string as SQL NULL for to_agent so the
	// IS NULL broadcast query works correctly.
	var toAgentVal interface{}
	if toAgent != "" {
		toAgentVal = toAgent
	}

	tx, err := s.knowledgeDB.Begin()
	if err != nil {
		return "", fmt.Errorf("send message: begin tx: %w", err)
	}

	if _, err := tx.Exec(
		`INSERT INTO agent_messages (id, from_agent, to_agent, topic, payload, project_id, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		id, fromAgent, toAgentVal, topic, payload, projectID, nowUnix,
	); err != nil {
		_ = tx.Rollback()
		return "", fmt.Errorf("send message: %w", err)
	}

	// Prune stale messages: read messages after 24 h, unread after 7 days.
	readCutoff := now.Add(-msgReadTTL).Unix()
	unreadCutoff := now.Add(-msgUnreadTTL).Unix()
	_, _ = tx.Exec(
		`DELETE FROM agent_messages WHERE (read_at IS NOT NULL AND created_at < ?) OR created_at < ?`,
		readCutoff, unreadCutoff,
	)

	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("send message: commit: %w", err)
	}
	return id, nil
}

// GetMessages returns messages visible to agentID with seq > sinceSeq.
// Visible means: addressed directly to agentID, OR broadcast (to_agent IS NULL).
// If topicFilter is non-empty, only messages with that exact topic are returned.
// If unreadOnly is true, only messages where read_at IS NULL are returned.
// Results are ordered by seq ASC (oldest first) so callers process in order.
// The returned latestSeq is the highest seq in the result set (use as next sinceSeq).
func (s *Store) GetMessages(agentID string, sinceSeq int64, topicFilter string, unreadOnly bool, limit int) ([]Message, int64, error) {
	if limit <= 0 {
		limit = 50
	}

	// Build query dynamically based on filters.
	// Broadcast messages (to_agent IS NULL) are always included alongside
	// direct messages (to_agent = agentID).
	query := `
		SELECT seq, id, from_agent, COALESCE(to_agent,''), topic, payload,
		       COALESCE(project_id,''), created_at, read_at
		FROM agent_messages
		WHERE seq > ?
		  AND (to_agent = ? OR to_agent IS NULL)`

	args := []interface{}{sinceSeq, agentID}

	if topicFilter != "" {
		query += ` AND topic = ?`
		args = append(args, topicFilter)
	}
	if unreadOnly {
		query += ` AND read_at IS NULL`
	}

	query += ` ORDER BY seq ASC LIMIT ?`
	args = append(args, limit)

	rows, err := s.knowledgeDB.Query(query, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("get messages: %w", err)
	}
	defer rows.Close()

	var msgs []Message
	var latestSeq int64
	for rows.Next() {
		var m Message
		var readAt sql.NullInt64
		if err := rows.Scan(
			&m.Seq, &m.ID, &m.FromAgent, &m.ToAgent,
			&m.Topic, &m.Payload, &m.ProjectID,
			&m.CreatedAt, &readAt,
		); err != nil {
			return nil, 0, fmt.Errorf("scan message: %w", err)
		}
		if readAt.Valid {
			v := readAt.Int64
			m.ReadAt = &v
		}
		if m.Seq > latestSeq {
			latestSeq = m.Seq
		}
		msgs = append(msgs, m)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("iterate messages: %w", err)
	}
	return msgs, latestSeq, nil
}

// CountUnreadMessages returns the number of unread messages visible to agentID
// (direct messages to the agent or broadcasts). Fast indexed count query.
func (s *Store) CountUnreadMessages(agentID string) (int, error) {
	var count int
	err := s.knowledgeDB.QueryRow(`
		SELECT COUNT(*) FROM agent_messages
		WHERE read_at IS NULL
		  AND (to_agent = ? OR to_agent IS NULL)`, agentID).Scan(&count)
	return count, err
}

// MarkRead stamps a direct message as read by the given agent.
// Only direct messages (to_agent matches) can be marked read — broadcasts
// (to_agent IS NULL) remain visible to all agents and cannot be marked read.
// Calling MarkRead on an already-read message is a no-op (idempotent).
func (s *Store) MarkRead(messageID, agentID string) error {
	now := time.Now().Unix()
	res, err := s.knowledgeDB.Exec(
		`UPDATE agent_messages
		 SET read_at = ?
		 WHERE id = ?
		   AND read_at IS NULL
		   AND to_agent = ?`,
		now, messageID, agentID,
	)
	if err != nil {
		return fmt.Errorf("mark read: %w", err)
	}
	// Not an error if 0 rows affected — message already read or not visible.
	_ = res
	return nil
}
