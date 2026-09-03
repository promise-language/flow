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
	Flow      string             `yaml:"flow"`
	Schema    int                `yaml:"schema"`
	SeededAt  time.Time          `yaml:"seeded_at"`
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
	// Replaced, not appended to, by a new ask: like Park, the field carries
	// the item's current state rather than its history, which the question
	// comments already hold. It is dropped with the park that was waiting on
	// it — a record that outlives its park is one the next question park
	// inherits, and an already-answered ask presented as the outstanding one
	// is worse than none, because `answer` accepts it.
	Questions []stateQuestionDoc `yaml:"questions,omitempty"`
	// Finalized marks the item's flow run as complete. Set by Finalize and
	// read back by LoadState into Item.Finalized so `status` can distinguish
	// "finalized" from "no flow currently eligible".
	Finalized bool `yaml:"finalized,omitempty"`
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
		Step:     req.Step,
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
		Step:    d.Step,
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
// AgentQuestion payload the Backend.AskQuestions contract requires a backend
// to persist, plus the id the backend assigned and the timestamp — GitHub's
// clock — that answer scanning compares replies against.
//
// No answer field: the issue thread IS the answer store (see ReadAnswers), so
// a copy of the answer here would be a second version of that fact to keep in
// step with the first.
type stateQuestionDoc struct {
	ID          string    `yaml:"id"`
	Header      string    `yaml:"header,omitempty"`
	Text        string    `yaml:"text,omitempty"`
	Format      string    `yaml:"format,omitempty"`
	Options     []string  `yaml:"options,omitempty"`
	MultiSelect bool      `yaml:"multi_select,omitempty"`
	AskedAt     time.Time `yaml:"asked_at,omitempty"`
}

func questionDocsFromRecorded(qs []flow.Question) []stateQuestionDoc {
	var docs []stateQuestionDoc
	for _, q := range qs {
		docs = append(docs, stateQuestionDoc{
			ID:          q.ID,
			Header:      q.Header,
			Text:        q.Text,
			Format:      string(q.Format),
			Options:     q.Options,
			MultiSelect: q.MultiSelect,
			AskedAt:     q.AskedAt,
		})
	}
	return docs
}

func questionsFromDocs(docs []stateQuestionDoc) []flow.Question {
	var qs []flow.Question
	for _, d := range docs {
		qs = append(qs, flow.Question{
			ID: d.ID,
			AgentQuestion: flow.AgentQuestion{
				Text:        d.Text,
				Header:      d.Header,
				Format:      flow.QuestionFormat(d.Format),
				Options:     d.Options,
				MultiSelect: d.MultiSelect,
			},
			AskedAt: d.AskedAt,
		})
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
		Id:                          flow.ArtifactId(d.Id),
		Type:                        artifactTypeFromString(d.Type),
		Required:                    d.Required,
		Stale:                       d.Stale,
		Resolved:                    d.Resolved,
		ResolvedBy:                  pickResolvedBy(d),
		ProducedAt:                  d.ProducedAt,
		Version:                     d.Version,
		CommitHash:                  d.CommitHash,
		GrantedInvocations:          d.GrantedInvocations,
		GrantedPromptsPerInvocation: d.GrantedPromptsPerInvocation,
		GrantedCostUSD:              d.GrantedCostUSD,
		GrantedTimeout:              d.GrantedTimeout,
		Invocations:                 d.Invocations,
		PromptsThisInvocation:       d.PromptsThisInvocation,
		CostUSDSpent:                d.CostUSDSpent,
		DurationWorked:              d.DurationWorked,
		LastRunAt:                   d.LastRunAt,
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
