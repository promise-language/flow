package github

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/promise-language/flow"
	"gopkg.in/yaml.v3"
)

// stateSchemaVersion is the int written into state-v1 documents. Bumped only
// on incompatible schema changes.
const stateSchemaVersion = 1

// stateBegin / stateEnd are the HTML-comment markers wrapping the YAML. The
// regex pulls the YAML out of the comment body.
var (
	stateBeginRe = regexp.MustCompile(`(?m)^<!--\s*flow:state-v1\s+begin(?:\s+owner=(\S+))?\s*-->`)
	stateEndRe   = regexp.MustCompile(`(?m)^<!--\s*flow:state-v1\s+end\s*-->`)
	yamlFenceRe  = regexp.MustCompile("(?s)```yaml\\s*\\n(.*?)\\n```")
)

// stateDoc is the on-wire YAML schema. Keep field names stable across
// versions; add new fields as optional rather than renaming.
type stateDoc struct {
	Flow     string    `yaml:"flow"`
	Schema   int       `yaml:"schema"`
	SeededAt time.Time `yaml:"seeded_at"`
	// Journal is the durable route: one entry per completed step execution, in
	// order (docs/github-schema.md § Journal entries). APPEND-ONLY — entries are
	// never rewritten, reordered or removed, and the last entry's `next` (or
	// `finalize`) is what the pending step is derived from.
	Journal []stateJournalEntryDoc `yaml:"journal,omitempty"`
	// Ledger is the treasurer's record: a row per step id, plus item-level
	// totals (docs/github-schema.md § Ledger).
	Ledger    stateLedgerDoc     `yaml:"ledger,omitempty"`
	Artifacts []stateArtifactDoc `yaml:"artifacts,omitempty"`
	Signals   []stateSignalDoc   `yaml:"signals,omitempty"`
	// Park is the item's current park, or nil when it is not parked. The
	// park label and the timeline comment Park() also writes are for humans
	// and for history; THIS is the machine-readable copy LoadState returns,
	// and it lives here — in the one comment LoadState already fetches — so
	// reading it costs no extra API call and a new park supersedes the old
	// one instead of accumulating.
	Park *stateParkDoc `yaml:"park,omitempty"`
	// Questions are the questions the item is currently parked on. Written by
	// AskQuestions and read back by LoadState, for the same reason Park is:
	// the question comment the ask also posts is for humans, and nothing can
	// reconstruct an id or a backend timestamp from it. Without this record
	// LoadState returns no questions at all, so `answer` — the only command
	// that clears a question park — refuses every item it is pointed at.
	//
	// APPENDED to, never replaced: questions are never removed — one leaves
	// the pending set by being ANSWERED, not by being deleted — so the record
	// of what was asked survives the answering. Replacing the list on a second
	// ask discarded every earlier unanswered question, leaving asks that had
	// silently ceased to exist while the item still waited on them.
	Questions []stateQuestionDoc `yaml:"questions,omitempty"`
	// Finalized marks the item's flow run as complete. Set by Finalize and
	// read back by LoadState into Item.Finalized so `status` can distinguish
	// "finalized" from "no flow currently eligible".
	Finalized bool `yaml:"finalized,omitempty"`
	// Disposition is how the finalizing election ended the flow — `resolved` or
	// `rejected` — present only when Finalized. The flow's decision, distinct
	// from GitHub's own state reason, which is read off the issue.
	Disposition string `yaml:"disposition,omitempty"`
}

// stateJournalEntryDoc is one completed step execution on the wire, shaped as
// docs/github-schema.md § Journal entries specifies.
//
// It carries `type` even though the flow already knows whether a step produces
// an artifact or a signal: a WIRE READER HAS NO FLOW, and the schema is written
// for readers who have no SDK. It is derived at write time.
type stateJournalEntryDoc struct {
	Step      string `yaml:"step"`
	Execution int    `yaml:"execution"`
	Type      string `yaml:"type,omitempty"`

	// Inline values for the small types. Large ones live in their own artifact
	// comment or on the orphan branch, reached through BodyAt.
	CommitHash string `yaml:"commit_hash,omitempty"`
	JSONInline string `yaml:"json,omitempty"`
	BodyAt     string `yaml:"body_at,omitempty"`

	// Next and Finalize are the election: exactly one is present.
	Next     string `yaml:"next,omitempty"`
	Awaits   string `yaml:"awaits,omitempty"`
	Finalize string `yaml:"finalize,omitempty"`

	Message string    `yaml:"message,omitempty"`
	Note    string    `yaml:"note,omitempty"`
	By      string    `yaml:"by,omitempty"`
	Role    string    `yaml:"role,omitempty"`
	At      time.Time `yaml:"at,omitempty"`

	CostUSD         float64 `yaml:"cost_usd,omitempty"`
	DurationSeconds float64 `yaml:"duration_seconds,omitempty"`
}

// awaitsSignalPrefix marks an awaited SIGNAL in the wire's one `awaits` string.
// A role and a signal are different kinds of wait — one is somebody's move, the
// other nobody's — and the wire carries one field, so the prefix is what tells
// them apart (docs/github-schema.md § Journal entries).
const awaitsSignalPrefix = "signal:"

// awaitsString renders an Awaits for the wire: the role name, or
// `signal:<id>`. Empty when the item awaits nothing.
func awaitsString(a flow.Awaits) string {
	if a.Signal != "" {
		return awaitsSignalPrefix + string(a.Signal)
	}
	return string(a.Role)
}

// awaitsFromString is the inverse. The Account half is NOT on the wire: it is
// read from the journal (Item.AccountForRole), so a stored copy would be a
// second answer to who holds the role.
func awaitsFromString(s string) flow.Awaits {
	if s == "" {
		return flow.Awaits{}
	}
	if rest, ok := strings.CutPrefix(s, awaitsSignalPrefix); ok {
		return flow.Awaits{Signal: flow.SignalId(rest)}
	}
	return flow.Awaits{Role: flow.RoleName(s)}
}

// awaitsFromDoc is the ONE derivation of what an item awaits: the last entry's
// marker, with the account of record for that role read back out of the
// journal. Load and the listing both go through it, so they cannot disagree
// about whose move it is.
//
// A finalized item awaits nobody, and neither does one with an empty journal.
// The account is never stored — "the binding is read from the journal, which
// already carries who ran every step" (docs/resolution.md § Whose move it is).
func awaitsFromDoc(doc *stateDoc) flow.Awaits {
	if doc == nil || doc.Finalized || len(doc.Journal) == 0 {
		return flow.Awaits{}
	}
	a := awaitsFromString(doc.Journal[len(doc.Journal)-1].Awaits)
	if a.Role == "" {
		return a
	}
	for i := len(doc.Journal) - 1; i >= 0; i-- {
		if doc.Journal[i].Role == string(a.Role) {
			a.Account = flow.AccountId(doc.Journal[i].By)
			break
		}
	}
	return a
}

// stateLedgerDoc is the treasurer's record on the wire.
type stateLedgerDoc struct {
	Steps map[string]stateLedgerRowDoc `yaml:"steps,omitempty"`

	TotalCostUSD         float64 `yaml:"total_cost_usd,omitempty"`
	TotalDurationSeconds float64 `yaml:"total_duration_seconds,omitempty"`
	TotalWaitingSeconds  float64 `yaml:"total_waiting_seconds,omitempty"`
}

type stateLedgerRowDoc struct {
	Dispatches   int     `yaml:"dispatches,omitempty"`
	Resumptions  int     `yaml:"resumptions,omitempty"`
	CostUSDSpent float64 `yaml:"cost_usd_spent,omitempty"`
	// DurationSeconds is ACTIVE time; WaitingSeconds is time blocked on a
	// declared exclusion. The two are separate on the wire because they are
	// separate facts: one is about the work, the other about contention.
	DurationSeconds float64               `yaml:"duration_seconds,omitempty"`
	WaitingSeconds  float64               `yaml:"waiting_seconds,omitempty"`
	Granted         []stateLedgerGrantDoc `yaml:"granted,omitempty"`
	LastRunAt       time.Time             `yaml:"last_run_at,omitempty"`
}

type stateLedgerGrantDoc struct {
	Axis   string    `yaml:"axis"`
	Amount float64   `yaml:"amount"`
	At     time.Time `yaml:"at,omitempty"`
}

// ledgerFromDoc inflates the read model. Durations are carried as seconds on
// the wire — a float a reader with no Go can interpret — and become Durations
// exactly here.
func ledgerFromDoc(d stateLedgerDoc) flow.Ledger {
	l := flow.Ledger{
		TotalCostUSD: d.TotalCostUSD,
		TotalActive:  secondsToDuration(d.TotalDurationSeconds),
		TotalWaiting: secondsToDuration(d.TotalWaitingSeconds),
	}
	if len(d.Steps) == 0 {
		return l
	}
	l.Steps = make(map[flow.StepId]flow.LedgerRow, len(d.Steps))
	for id, row := range d.Steps {
		out := flow.LedgerRow{
			Step:        flow.StepId(id),
			Dispatches:  row.Dispatches,
			Resumptions: row.Resumptions,
			CostUSD:     row.CostUSDSpent,
			Active:      secondsToDuration(row.DurationSeconds),
			Waiting:     secondsToDuration(row.WaitingSeconds),
			LastRunAt:   row.LastRunAt,
		}
		for _, g := range row.Granted {
			out.Granted = append(out.Granted, flow.GrantRecord{
				Axis:   flow.BudgetAxis(g.Axis),
				Amount: g.Amount,
				At:     g.At,
			})
		}
		l.Steps[flow.StepId(id)] = out
	}
	return l
}

// secondsToDuration scales as a float rather than converting through an integer
// second count, which truncates: a step that ran for 300ms would otherwise read
// back as zero.
func secondsToDuration(secs float64) time.Duration {
	return time.Duration(secs * float64(time.Second))
}

// journalEntryDocOf renders one entry for the wire. bodyAt is the URL of the
// comment or spill file carrying the payload, empty for a value stored inline
// or for a step that produced none.
func journalEntryDocOf(e flow.JournalEntry, bodyAt string) stateJournalEntryDoc {
	d := stateJournalEntryDoc{
		Step:            string(e.Step),
		Execution:       e.Execution,
		Next:            string(e.Route.Next),
		Awaits:          awaitsString(e.Awaits),
		Finalize:        string(e.Route.Finalize),
		Message:         e.Message,
		Note:            e.Note,
		By:              string(e.By),
		Role:            string(e.Role),
		At:              e.At,
		CostUSD:         e.Spend.CostUSD,
		DurationSeconds: e.Spend.Duration.Seconds(),
		BodyAt:          bodyAt,
	}
	// `type` discriminates the result kind for a reader with no flow: one of the
	// artifact types, or `signal` where the result is the observation itself.
	if e.Result.Type == 0 {
		d.Type = journalSignalType
	} else {
		d.Type = artifactTypeString(e.Result.Type)
	}
	if e.Result.Type == flow.ArtifactCommitHash {
		d.CommitHash = e.Result.CommitHash
	}
	if e.Result.Type == flow.ArtifactJSON {
		d.JSONInline = string(e.Result.JSON)
	}
	return d
}

// journalSignalType is the wire's `type` for an entry whose result is an
// observation rather than an artifact value.
const journalSignalType = "signal"

// journalEntryFromDoc inflates one entry. The payload of a spilled or
// comment-stored artifact is NOT inlined here — the artifact projection carries
// it, hydrated on load the way it always was.
func journalEntryFromDoc(d stateJournalEntryDoc) flow.JournalEntry {
	e := flow.JournalEntry{
		Step:      flow.StepId(d.Step),
		Execution: d.Execution,
		Route:     flow.Route{Next: flow.StepId(d.Next), Finalize: flow.Disposition(d.Finalize)},
		Awaits:    awaitsFromString(d.Awaits),
		Message:   d.Message,
		Note:      d.Note,
		By:        flow.AccountId(d.By),
		Role:      flow.RoleName(d.Role),
		At:        d.At,
		Spend: flow.Spend{
			CostUSD:  d.CostUSD,
			Duration: secondsToDuration(d.DurationSeconds),
		},
	}
	if d.Type != "" && d.Type != journalSignalType {
		e.Result.Type = artifactTypeFromString(d.Type)
		e.Result.CommitHash = d.CommitHash
		if d.JSONInline != "" {
			e.Result.JSON = []byte(d.JSONInline)
		}
	}
	return e
}

type stateParkDoc struct {
	Kind string `yaml:"kind"`
	Step string `yaml:"step,omitempty"` // step ID (artifact/signal id)
	Axis string `yaml:"axis,omitempty"`
	// Axes is the every-axis snapshot behind ParkRequest.Axes. Persisted so
	// the operator reading a park hours later sees the same full picture the
	// run did, instead of the one axis that happened to trip first.
	Axes     []stateParkAxisDoc `yaml:"axes,omitempty"`
	Reason   string             `yaml:"reason,omitempty"`
	Details  string             `yaml:"details,omitempty"`
	ParkedAt time.Time          `yaml:"parked_at,omitempty"`
}

type stateParkAxisDoc struct {
	Axis    string  `yaml:"axis"`
	Used    float64 `yaml:"used,omitempty"`
	Granted float64 `yaml:"granted,omitempty"`
	// Exhausted is persisted rather than recomputed: it records the run's own
	// verdict at park time, which is the thing being reported.
	Exhausted bool `yaml:"exhausted,omitempty"`
}

func parkDocFromRequest(req flow.ParkRequest, at time.Time) *stateParkDoc {
	doc := &stateParkDoc{
		Kind:     string(req.Kind),
		Step:     string(req.Step),
		Axis:     string(req.Axis),
		Reason:   req.Reason,
		Details:  req.Details,
		ParkedAt: at,
	}
	for _, a := range req.Axes {
		doc.Axes = append(doc.Axes, stateParkAxisDoc{
			Axis:      string(a.Axis),
			Used:      a.Used,
			Granted:   a.Granted,
			Exhausted: a.Exhausted,
		})
	}
	return doc
}

func parkRequestFromDoc(d *stateParkDoc) *flow.ParkRequest {
	if d == nil {
		return nil
	}
	req := &flow.ParkRequest{
		Kind:    flow.ParkKind(d.Kind),
		Step:    flow.StepId(d.Step),
		Axis:    flow.BudgetAxis(d.Axis),
		Reason:  d.Reason,
		Details: d.Details,
	}
	for _, a := range d.Axes {
		req.Axes = append(req.Axes, flow.AxisReport{
			Axis:      flow.BudgetAxis(a.Axis),
			Used:      a.Used,
			Granted:   a.Granted,
			Exhausted: a.Exhausted,
		})
	}
	return req
}

// stateQuestionDoc is one recorded question. It carries the whole
// AgentQuestion payload the Orchestrator.AskQuestion contract requires an
// orchestrator to persist, plus the QuestionId it assigned and the timestamp —
// GitHub's clock — that answer scanning compares replies against.
//
// The answer is recorded HERE as well as in the thread. The thread is where a
// person reads it; this is where `answer` and Load read it, and it is the only
// copy that can say WHICH question a reply answered — a comment carries no
// question id, so a thread-only store leaves every question pending forever.
type stateQuestionDoc struct {
	ID string `yaml:"id"`
	// Answer and AnsweredAt are written by PostAnswer, against the question
	// they answer. Without them an answer landed nowhere: the question stayed
	// pending forever, and the marker cleared on the first of several.
	Answer      string    `yaml:"answer,omitempty"`
	AnsweredAt  time.Time `yaml:"answered_at,omitempty"`
	Header      string    `yaml:"header,omitempty"`
	Text        string    `yaml:"text,omitempty"`
	Format      string    `yaml:"format,omitempty"`
	Options     []string  `yaml:"options,omitempty"`
	MultiSelect bool      `yaml:"multi_select,omitempty"`
	AskedAt     time.Time `yaml:"asked_at,omitempty"`
}

// questionDocOf renders one recorded question for the state doc.
func questionDocOf(q flow.Question) stateQuestionDoc {
	d := stateQuestionDoc{
		ID:          string(q.ID),
		Header:      q.Header,
		Text:        q.Text,
		Format:      string(q.Format),
		Options:     q.Options,
		MultiSelect: q.MultiSelect,
		AskedAt:     q.AskedAt,
		Answer:      q.Answer,
	}
	if q.AnsweredAt != nil {
		d.AnsweredAt = *q.AnsweredAt
	}
	return d
}

func questionsFromDocs(docs []stateQuestionDoc) []flow.Question {
	var qs []flow.Question
	for _, d := range docs {
		q := flow.Question{
			ID: flow.QuestionId(d.ID),
			AgentQuestion: flow.AgentQuestion{
				Text:        d.Text,
				Header:      d.Header,
				Format:      flow.QuestionFormat(d.Format),
				Options:     d.Options,
				MultiSelect: d.MultiSelect,
			},
			UserAnswer: flow.UserAnswer{Answer: d.Answer},
			AskedAt:    d.AskedAt,
		}
		if !d.AnsweredAt.IsZero() {
			at := d.AnsweredAt
			q.AnsweredAt = &at
		}
		qs = append(qs, q)
	}
	return qs
}

type stateArtifactDoc struct {
	Id                  string    `yaml:"id"`
	Type                string    `yaml:"type"`
	Required            bool      `yaml:"required,omitempty"`
	Stale               bool      `yaml:"stale,omitempty"`
	Resolved            bool      `yaml:"resolved,omitempty"`
	ResolvedBy          string    `yaml:"resolved_by,omitempty"`
	ProducedAt          time.Time `yaml:"produced_at,omitempty"`
	Version             int       `yaml:"version,omitempty"`
	ResolvedByPrincipal string    `yaml:"resolved_by_principal,omitempty"`

	// inline value (small types) — large types (file/patch) live as
	// follow-up comments / orphan-branch files referenced by ResolvedBy.
	CommitHash string `yaml:"commit_hash,omitempty"`
	JSONInline string `yaml:"json,omitempty"`

	// budget caps
	GrantedInvocations          int           `yaml:"granted_invocations,omitempty"`
	GrantedPromptsPerInvocation int           `yaml:"granted_prompts_per_invocation,omitempty"`
	GrantedCostUSD              float64       `yaml:"granted_cost_usd,omitempty"`
	GrantedTimeout              time.Duration `yaml:"granted_timeout,omitempty"`

	// usage counters
	Invocations           int           `yaml:"invocations,omitempty"`
	PromptsThisInvocation int           `yaml:"prompts_this_invocation,omitempty"`
	CostUSDSpent          float64       `yaml:"cost_usd_spent,omitempty"`
	DurationWorked        time.Duration `yaml:"duration_worked,omitempty"`
	LastRunAt             time.Time     `yaml:"last_run_at,omitempty"`
}

type stateSignalDoc struct {
	Id          string    `yaml:"id"`
	Set         bool      `yaml:"set"`
	ObservedAt  time.Time `yaml:"observed_at,omitempty"`
	ObservedVia string    `yaml:"observed_via,omitempty"` // side-effect | poll
}

// extractStateDoc scans a comment body for the state-v1 markers and parses
// the YAML between them. Returns (doc, owner, true) on success. The owner
// is parsed from the `begin owner=<login>` attribute.
func extractStateDoc(body string) (*stateDoc, string, bool, error) {
	beginMatch := stateBeginRe.FindStringSubmatchIndex(body)
	if beginMatch == nil {
		return nil, "", false, nil
	}
	endMatch := stateEndRe.FindStringIndex(body[beginMatch[1]:])
	if endMatch == nil {
		return nil, "", true, errors.New("found state-v1 begin without matching end marker")
	}
	owner := ""
	if beginMatch[2] >= 0 {
		owner = body[beginMatch[2]:beginMatch[3]]
	}
	inner := body[beginMatch[1] : beginMatch[1]+endMatch[0]]

	// Grab the YAML inside the ```yaml ... ``` fence.
	yamlMatch := yamlFenceRe.FindStringSubmatch(inner)
	if yamlMatch == nil {
		return nil, owner, true, errors.New("state-v1 block missing ```yaml fence")
	}
	var doc stateDoc
	if err := yaml.Unmarshal([]byte(yamlMatch[1]), &doc); err != nil {
		return nil, owner, true, fmt.Errorf("state-v1 YAML: %w", err)
	}
	return &doc, owner, true, nil
}

// renderStateComment composes the full comment body (markers + <details>
// wrapper + ```yaml fence + payload).
func renderStateComment(owner string, doc stateDoc) (string, error) {
	body, err := yaml.Marshal(doc)
	if err != nil {
		return "", fmt.Errorf("marshal state doc: %w", err)
	}
	binary := doc.Flow
	var sb strings.Builder
	if owner == "" {
		sb.WriteString("<!-- flow:state-v1 begin -->\n")
	} else {
		fmt.Fprintf(&sb, "<!-- flow:state-v1 begin owner=%s -->\n", owner)
	}
	fmt.Fprintf(&sb, "<details><summary>📋 Flow state — %s (machine-managed, do not edit)</summary>\n\n", binary)
	sb.WriteString("```yaml\n")
	sb.Write(body)
	sb.WriteString("```\n\n")
	sb.WriteString("</details>\n")
	sb.WriteString("<!-- flow:state-v1 end -->\n")
	return sb.String(), nil
}

// NOTE: there is deliberately no docFromState/artifactDocFromRecord pair here.
// Rebuilding the document from a flow.ItemState carries only what ItemState
// models and silently drops the rest — the park record most importantly — so
// every write path edits the loaded document in place via mutateStateDoc.

// recordFromArtifactDoc inflates an ArtifactRecord from the YAML doc. The
// File / Patch payloads aren't inlined; the backend fetches them on demand
// from the comment / orphan branch.
func recordFromArtifactDoc(d stateArtifactDoc) flow.ArtifactRecord {
	rec := flow.ArtifactRecord{
		Id:         flow.ArtifactId(d.Id),
		Type:       artifactTypeFromString(d.Type),
		Resolved:   d.Resolved,
		ResolvedBy: pickResolvedBy(d),
		ProducedAt: d.ProducedAt,
		Version:    d.Version,
		CommitHash: d.CommitHash,
	}
	if d.JSONInline != "" {
		rec.JSON = []byte(d.JSONInline)
	}
	return rec
}

func pickResolvedBy(d stateArtifactDoc) string {
	if d.ResolvedByPrincipal != "" {
		return d.ResolvedByPrincipal
	}
	return d.ResolvedBy
}

// signalStateFromDoc inflates a SignalState from the doc.
func signalStateFromDoc(d stateSignalDoc) flow.SignalState {
	return flow.SignalState{
		Set:        d.Set,
		ObservedAt: d.ObservedAt,
		By:         d.ObservedVia,
	}
}

func artifactTypeString(t flow.ArtifactType) string {
	switch t {
	case flow.ArtifactFlag:
		return "flag"
	case flow.ArtifactCommitHash:
		return "commit_hash"
	case flow.ArtifactMarkdown:
		return "markdown"
	case flow.ArtifactJSON:
		return "json"
	case flow.ArtifactFile:
		return "file"
	case flow.ArtifactPatch:
		return "patch"
	}
	return ""
}

func artifactTypeFromString(s string) flow.ArtifactType {
	switch s {
	case "flag":
		return flow.ArtifactFlag
	case "commit_hash", "commit-hash":
		return flow.ArtifactCommitHash
	case "markdown":
		return flow.ArtifactMarkdown
	case "json":
		return flow.ArtifactJSON
	case "file":
		return flow.ArtifactFile
	case "patch":
		return flow.ArtifactPatch
	}
	return 0
}
