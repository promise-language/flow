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
}

type stateArtifactDoc struct {
	Id           string    `yaml:"id"`
	Type         string    `yaml:"type"`
	Required     bool      `yaml:"required,omitempty"`
	Stale        bool      `yaml:"stale,omitempty"`
	Resolved     bool      `yaml:"resolved,omitempty"`
	ResolvedBy   string    `yaml:"resolved_by,omitempty"`
	ProducedAt   time.Time `yaml:"produced_at,omitempty"`
	Version      int       `yaml:"version,omitempty"`
	ResolvedByPrincipal string `yaml:"resolved_by_principal,omitempty"`

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
	Invocations           int       `yaml:"invocations,omitempty"`
	PromptsThisInvocation int       `yaml:"prompts_this_invocation,omitempty"`
	CostUSDSpent          float64   `yaml:"cost_usd_spent,omitempty"`
	LastRunAt             time.Time `yaml:"last_run_at,omitempty"`
}

type stateSignalDoc struct {
	Id           string    `yaml:"id"`
	Set          bool      `yaml:"set"`
	ObservedAt   time.Time `yaml:"observed_at,omitempty"`
	ObservedVia  string    `yaml:"observed_via,omitempty"` // side-effect | poll
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

// docFromState builds a stateDoc from a flow.ItemState snapshot. Used when
// the backend wants to persist a freshly-observed state.
func docFromState(flowName string, state *flow.ItemState, seededAt time.Time) stateDoc {
	doc := stateDoc{
		Flow:     flowName,
		Schema:   stateSchemaVersion,
		SeededAt: seededAt,
	}
	for id, rec := range state.Artifacts {
		doc.Artifacts = append(doc.Artifacts, artifactDocFromRecord(id, rec))
	}
	for id, sig := range state.Signals {
		doc.Signals = append(doc.Signals, stateSignalDoc{
			Id:          string(id),
			Set:         sig.Set,
			ObservedAt:  sig.ObservedAt,
			ObservedVia: sig.By,
		})
	}
	return doc
}

func artifactDocFromRecord(id flow.ArtifactId, rec flow.ArtifactRecord) stateArtifactDoc {
	d := stateArtifactDoc{
		Id:                          string(id),
		Type:                        artifactTypeString(rec.Type),
		Required:                    rec.Required,
		Stale:                       rec.Stale,
		Resolved:                    rec.Resolved,
		ResolvedBy:                  rec.ResolvedBy,
		ProducedAt:                  rec.ProducedAt,
		Version:                     rec.Version,
		ResolvedByPrincipal:         rec.ResolvedBy, // duplicated for human-readable column
		CommitHash:                  rec.CommitHash,
		GrantedInvocations:          rec.GrantedInvocations,
		GrantedPromptsPerInvocation: rec.GrantedPromptsPerInvocation,
		GrantedCostUSD:              rec.GrantedCostUSD,
		GrantedTimeout:              rec.GrantedTimeout,
		Invocations:                 rec.Invocations,
		PromptsThisInvocation:       rec.PromptsThisInvocation,
		CostUSDSpent:                rec.CostUSDSpent,
		LastRunAt:                   rec.LastRunAt,
	}
	if rec.Type == flow.ArtifactJSON && len(rec.JSON) > 0 {
		d.JSONInline = string(rec.JSON)
	}
	return d
}

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
