package ice

import "time"

// SourceKind identifies where a context chunk came from.
type SourceKind int

const (
	SourceConversation SourceKind = iota
	SourceMemory
)

// ConversationEntry is a single stored message with its embedding.
type ConversationEntry struct {
	ID         int       `json:"id"`
	ProjectID  string    `json:"project_id,omitempty"`
	SessionID  string    `json:"session_id"`
	Role       string    `json:"role"` // "user", "assistant", "summary"
	Content    string    `json:"content"`
	Embedding  []float32 `json:"embedding"`
	EmbedModel string    `json:"embed_model,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	TurnIndex  int       `json:"turn_index"`
}

// ScoredEntry pairs a conversation entry with its similarity score.
type ScoredEntry struct {
	Entry ConversationEntry
	Score float32
}

// ContextChunk is a piece of assembled context ready for the prompt.
type ContextChunk struct {
	Source  SourceKind
	Content string
	Score   float32
	Tokens  int
}

// Budget holds the token allocation for each context source.
type Budget struct {
	Total        int
	System       int
	Conversation int
	Memory       int
	Recent       int
}
