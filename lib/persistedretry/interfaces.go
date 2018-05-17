package persistedretry

// Task represents a single unit of work which must eventually succeed.
type Task interface{}

// Store provides persisted storage for tasks.
type Store interface {
	GetFailed() ([]Task, error)
	GetPending() ([]Task, error)
	MarkPending(Task) error
	MarkFailed(Task) error
	MarkDone(Task) error
	Close() error
}

// Executor executes tasks.
type Executor interface {
	Exec(Task) error
}
