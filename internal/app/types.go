package app

import "time"

type Project struct {
	Directory   string `json:"directory,omitempty"`
	FolderState string `json:"folderState,omitempty"`
	FolderError string `json:"folderError,omitempty"`
	ID          string `json:"id"`
	Title       string `json:"title"`
	Brief       string `json:"brief"`
	CreatedAt   string `json:"createdAt"`
	UpdatedAt   string `json:"updatedAt"`
}

type Message struct {
	RequestedAgent string `json:"requestedAgent,omitempty"`
	SelfReview     bool   `json:"selfReview,omitempty"`
	Model          string `json:"model,omitempty"`
	ID             string `json:"id"`
	ProjectID      string `json:"projectId"`
	RunID          string `json:"runId"`
	Actor          string `json:"actor"`
	Target         string `json:"target"`
	Text           string `json:"text"`
	Stage          string `json:"stage"`
	Demo           bool   `json:"demo"`
	CreatedAt      string `json:"createdAt"`
}

type Material struct {
	ID        string `json:"id"`
	ProjectID string `json:"projectId"`
	Name      string `json:"name"`
	Role      string `json:"role"`
	MIME      string `json:"mime"`
	Size      int64  `json:"size"`
	SHA256    string `json:"sha256"`
	Text      string `json:"text"`
	Warning   string `json:"warning"`
	CreatedAt string `json:"createdAt"`
}

type Task struct {
	RequestedAgent string `json:"requestedAgent,omitempty"`
	SelfReview     bool   `json:"selfReview,omitempty"`
	ArtifactTaskID string `json:"artifactTaskId,omitempty"`
	BuildID        string `json:"buildId,omitempty"`
	Model          string `json:"model,omitempty"`
	ID             string `json:"id"`
	Agent          string `json:"agent"`
	Stage          string `json:"stage"`
	Phase          int    `json:"phase"`
	Status         string `json:"status"`
	Output         string `json:"output"`
	StartedAt      string `json:"startedAt,omitempty"`
	FinishedAt     string `json:"finishedAt,omitempty"`
}

type Run struct {
	Waiting            map[string]WaitState  `json:"waiting,omitempty"`
	Recovery           *RecoveryState        `json:"recovery,omitempty"`
	AvailabilityPolicy string                `json:"availabilityPolicy,omitempty"`
	Unavailable        map[string]AgentIssue `json:"unavailable,omitempty"`
	NeedsReview        bool                  `json:"needsReview,omitempty"`
	Audits             []IndependentReview   `json:"audits,omitempty"`
	ActiveAuditID      string                `json:"activeAuditId,omitempty"`
	AuditReturnStatus  string                `json:"auditReturnStatus,omitempty"`
	Prototype          *Prototype            `json:"prototype,omitempty"`
	Models             map[string]string     `json:"models,omitempty"`
	LockedModels       map[string]string     `json:"lockedModels,omitempty"`
	ParentRunID        string                `json:"parentRunId,omitempty"`
	Continuation       string                `json:"continuation,omitempty"`
	StopReason         string                `json:"stopReason,omitempty"`
	MaxRevisions       int                   `json:"maxRevisions"`
	Acceptance         string                `json:"acceptance"`
	Quality            string                `json:"quality"`
	ID                 string                `json:"id"`
	ProjectID          string                `json:"projectId"`
	Mode               string                `json:"mode"`
	Kind               string                `json:"kind"`
	Status             string                `json:"status"`
	Brief              string                `json:"brief"`
	Title              string                `json:"title"`
	Tasks              []Task                `json:"tasks"`
	Sessions           map[string]string     `json:"sessions"`
	Materials          []Material            `json:"materials"`
	Document           string                `json:"document"`
	Error              string                `json:"error"`
	CreatedAt          string                `json:"createdAt"`
	UpdatedAt          string                `json:"updatedAt"`
}

type Event struct {
	ID        int64  `json:"id"`
	RunID     string `json:"runId"`
	Agent     string `json:"agent"`
	Kind      string `json:"kind"`
	Text      string `json:"text"`
	CreatedAt string `json:"createdAt"`
}

type AgentStatus struct {
	AccountKey    string       `json:"-"`
	Health        *AgentHealth `json:"health,omitempty"`
	Agent         string       `json:"agent"`
	Installed     bool         `json:"installed"`
	Authenticated bool         `json:"authenticated"`
	Subscription  bool         `json:"subscription"`
	Version       string       `json:"version"`
	Detail        string       `json:"detail"`
	LoginCommand  string       `json:"loginCommand"`
}

func now() string { return time.Now().UTC().Format(time.RFC3339Nano) }
