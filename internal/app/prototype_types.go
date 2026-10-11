package app

// These records are persisted with the Run. The document snapshot and approved
// plan are immutable throughout an implementation/feedback cycle.
type Prototype struct {
	SourceCommit      string          `json:"sourceCommit,omitempty"`
	RepositoryContext string          `json:"repositoryContext,omitempty"`
	Source            Revision        `json:"source"`
	Config            PrototypeConfig `json:"config"`
	Plan              *PrototypePlan  `json:"plan,omitempty"`
	PlanHash          string          `json:"planHash"`
	ApprovedAt        string          `json:"approvedAt,omitempty"`
	Workspace         string          `json:"workspace"`
	BaseCommit        string          `json:"baseCommit,omitempty"`
	Builds            []Build         `json:"builds"`
	Decisions         []Decision      `json:"decisions"`
	Tools             []ToolCheck     `json:"tools"`
	ActiveFrom        int             `json:"activeFrom"`
	RoundLimit        int             `json:"roundLimit"`
	ActiveSince       string          `json:"activeSince,omitempty"`
	UsedSeconds       float64         `json:"usedSeconds"`
	FailureCount      int             `json:"failureCount"`
	Feedback          string          `json:"feedback,omitempty"`
	CurrentCommit     string          `json:"currentCommit,omitempty"`
}
type PrototypeConfig struct {
	RevisionID      string `json:"revisionId"`
	Platform        string `json:"platform"`
	ProductPlatform string `json:"productPlatform"`
	Hypothesis      string `json:"hypothesis"`
	SourceRepo      string `json:"sourceRepo"`
	ParentDirectory string `json:"parentDirectory"`
	Author          string `json:"author"`
	MaxRounds       int    `json:"maxRounds"`
	MaxMinutes      int    `json:"maxMinutes"`
	MaxFailures     int    `json:"maxFailures"`
}
type PrototypePlan struct {
	PreviewDirectory string         `json:"previewDirectory,omitempty"`
	Title            string         `json:"title"`
	Platform         string         `json:"platform"`
	Stack            string         `json:"stack"`
	Hypothesis       string         `json:"hypothesis"`
	Scope            []string       `json:"scope"`
	Simplifications  []string       `json:"simplifications"`
	Acceptance       []string       `json:"acceptance"`
	Tools            []string       `json:"tools"`
	Checks           []CheckCommand `json:"checks"`
	RunInstructions  string         `json:"runInstructions"`
	ManualChecks     []string       `json:"manualChecks"`
}
type CheckCommand struct {
	Name           string   `json:"name"`
	Kind           string   `json:"kind"` // setup, build, test
	Args           []string `json:"args"`
	TimeoutSeconds int      `json:"timeoutSeconds"`
}
type ToolCheck struct {
	Name      string `json:"name"`
	Available bool   `json:"available"`
	Detail    string `json:"detail"`
}
type Decision struct {
	CreatedAt string `json:"createdAt"`
	Actor     string `json:"actor"`
	BuildID   string `json:"buildId,omitempty"`
	Text      string `json:"text"`
}
type FileEdit struct {
	Path    string `json:"path"`
	Content string `json:"content"`
	Delete  bool   `json:"delete,omitempty"`
}
type CodeProposal struct {
	Summary     string     `json:"summary"`
	Decisions   []string   `json:"decisions"`
	Limitations []string   `json:"limitations"`
	Files       []FileEdit `json:"files"`
}
type CodeReview struct {
	Ready        bool     `json:"ready"`
	Summary      string   `json:"summary"`
	Issues       []string `json:"issues"`
	ManualChecks []string `json:"manualChecks"`
}
type CheckResult struct {
	Name       string   `json:"name"`
	Kind       string   `json:"kind"`
	Args       []string `json:"args"`
	Status     string   `json:"status"`
	ExitCode   int      `json:"exitCode"`
	Output     string   `json:"output"`
	StartedAt  string   `json:"startedAt"`
	FinishedAt string   `json:"finishedAt,omitempty"`
}
type Build struct {
	PreviewReady   bool          `json:"previewReady"`
	PreviewError   string        `json:"previewError,omitempty"`
	ID             string        `json:"id"`
	TaskID         string        `json:"taskId"`
	Number         int           `json:"number"`
	CreatedAt      string        `json:"createdAt"`
	Commit         string        `json:"commit"`
	Diff           string        `json:"diff"`
	Summary        string        `json:"summary"`
	Limitations    []string      `json:"limitations"`
	Checks         []CheckResult `json:"checks"`
	Review         *CodeReview   `json:"review,omitempty"`
	AcceptedAt     string        `json:"acceptedAt,omitempty"`
	AcceptanceNote string        `json:"acceptanceNote,omitempty"`
}
