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
func (s *Store) SendMessage(fromAgent, toAgent, topic, payload, projectID string) (string, error) {
	id := newID()
	now := time.Now().Unix()

	// Normalise: store empty string as SQL NULL for to_agent so the
	// IS NULL broadcast query works correctly.
	var toAgentVal interface{}
	if toAgent != "" {
		toAgentVal = toAgent
	}

	_, err := s.db.Exec(
		`INSERT INTO agent_messages (id, from_agent, to_agent, topic, payload, project_id, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		id, fromAgent, toAgentVal, topic, payload, projectID, now,
	)
	if err != nil {
		return "", fmt.Errorf("send message: %w", err)
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

	rows, err := s.db.Query(query, args...)
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

// MarkRead stamps a message as read by the given agent.
// Only messages visible to agentID (direct or broadcast) can be marked read.
// Calling MarkRead on an already-read message is a no-op (idempotent).
func (s *Store) MarkRead(messageID, agentID string) error {
	now := time.Now().Unix()
	res, err := s.db.Exec(
		`UPDATE agent_messages
		 SET read_at = ?
		 WHERE id = ?
		   AND read_at IS NULL
		   AND (to_agent = ? OR to_agent IS NULL)`,
		now, messageID, agentID,
	)
	if err != nil {
		return fmt.Errorf("mark read: %w", err)
	}
	// Not an error if 0 rows affected — message already read or not visible.
	_ = res
	return nil
}
