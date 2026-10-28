package verify

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"autodoc/internal/gofacts"
	"autodoc/internal/guard"
	"autodoc/internal/strip"
)

// Swag is pinned: the spec must be reproducible, and the version is part of
// what gate A proves.
const (
	SwagVersion = "v1.16.6"
	SwagModule  = "github.com/swaggo/swag/cmd/swag"
)

// Issue is one gate finding, phrased so the producing agent can act on it.
type Issue struct {
	Check   string `json:"check"` // swag, code, scope, go, spec or constraint
	Where   string `json:"where,omitempty"`
	Message string `json:"message"`
}

// Report is the outcome of one gate run.
type Report struct {
	Gate   string        `json:"gate"`
	Checks []CheckResult `json:"checks"`
	Issues []Issue       `json:"issues"`
	// Advisory is set when the issues left do not fail the run: only
	// constraint issues on gate A's last attempt.
	Advisory bool `json:"advisory,omitempty"`
}

// OK reports whether the gate passed.
func (r Report) OK() bool { return len(r.Issues) == 0 }

// String renders the issues as the text that goes back to the agent.
func (r Report) String() string {
	if r.OK() {
		return fmt.Sprintf("gate %s passed", r.Gate)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "gate %s failed with %d issue(s):\n", r.Gate, len(r.Issues))
	for i, is := range r.Issues {
		where := ""
		if is.Where != "" {
			where = is.Where + ": "
		}
		msg := strings.ReplaceAll(is.Message, "\n", "\n   ")
		fmt.Fprintf(&b, "%d. [%s] %s%s\n", i+1, is.Check, where, msg)
	}
	return b.String()
}

// GateAInput is what gate A checks the working copy against.
type GateAInput struct {
	Dir        string
	Main       string                           // file with func main and the general API info, relative to Dir
	Baseline   guard.Snapshot                   // from baseline.json
	BaseChecks []CheckResult                    // from baseline.json
	RunTests   bool                             // also run the target's tests, as an advisory check
	Original   func(rel string) ([]byte, error) // a file's content at the base commit
	Facts      *gofacts.Facts
}

// Swag runs the pinned swag v1 in dir, reading the general API info from
// main, and writes swagger.json and swagger.yaml into out (relative to dir,
// or absolute). No docs.go is generated, so the target needs no swag
// dependency.
func Swag(ctx context.Context, dir, main, out string) CheckResult {
	return runGo(ctx, dir, "swag", "run", SwagModule+"@"+SwagVersion,
		"init", "-g", main, "--parseInternal", "--outputTypes", "json,yaml", "-o", out)
}

// GateA generates the spec and checks the working copy: code unchanged, only
// allowed files changed, no check that was green at the baseline now red,
// the spec valid and reproducible, and the spec in agreement with
// facts.json, its param defaults, enums and bounds included. Every check
// runs, so one report lists every problem.
func GateA(ctx context.Context, in GateAInput) Report {
	rep := Report{Gate: "A", Issues: []Issue{}}
	add := func(is ...Issue) { rep.Issues = append(rep.Issues, is...) }

	gen := Swag(ctx, in.Dir, in.Main, strip.SpecDir)
	rep.Checks = append(rep.Checks, gen)
	if !gen.OK {
		add(Issue{Check: "swag", Where: gen.Cmd, Message: "swag init failed; fix the annotation it names:\n" + gen.Output})
	} else {
		add(checkGenerated(ctx, in, &rep)...)
	}

	cur, err := guard.Take(in.Dir)
	if err != nil {
		add(Issue{Check: "code", Message: "snapshot the working copy: " + err.Error()})
	} else {
		add(checkCode(in.Baseline, cur)...)
		add(checkScope(in.Baseline, cur, in.Dir, in.Original)...)
	}

	// Without a spec, a build fails only because the apispec.go an earlier
	// run added embeds the missing swagger.json: on a re-run, strip deletes
	// it and swag writes it again. Only comments changed, so the build is
	// checked once swag succeeds.
	if _, err := os.Stat(filepath.Join(in.Dir, filepath.FromSlash(strip.SpecDir), "swagger.json")); !gen.OK && err != nil {
		return rep
	}
	checks := Checks(ctx, in.Dir, in.RunTests)
	rep.Checks = append(rep.Checks, checks...)
	for _, c := range Regressions(in.BaseChecks, checks) {
		add(Issue{Check: "go", Where: c.Cmd, Message: c.Output})
	}
	return rep
}

// checkGenerated validates swag's output, regenerates it into a temporary
// directory to prove it is reproducible, and compares it with the facts.
func checkGenerated(ctx context.Context, in GateAInput, rep *Report) []Issue {
	specDir := filepath.Join(in.Dir, filepath.FromSlash(strip.SpecDir))
	jsonData, err1 := os.ReadFile(filepath.Join(specDir, "swagger.json"))
	yamlData, err2 := os.ReadFile(filepath.Join(specDir, "swagger.yaml"))
	if err1 != nil || err2 != nil {
		return []Issue{{Check: "swag", Message: "swag did not write swagger.json and swagger.yaml into " + strip.SpecDir}}
	}
	out := validateSpec(jsonData, yamlData)

	tmp, err := os.MkdirTemp("", "autodoc-swag-")
	if err != nil {
		return append(out, Issue{Check: "swag", Message: "temp dir: " + err.Error()})
	}
	defer os.RemoveAll(tmp)
	again := Swag(ctx, in.Dir, in.Main, tmp)
	again.Name = "swag-regenerate"
	rep.Checks = append(rep.Checks, again)
	if !again.OK {
		out = append(out, Issue{Check: "swag", Message: "regenerating the spec failed:\n" + again.Output})
	} else {
		for name, want := range map[string][]byte{"swagger.json": jsonData, "swagger.yaml": yamlData} {
			got, err := os.ReadFile(filepath.Join(tmp, name))
			if err != nil || !bytes.Equal(got, want) {
				out = append(out, Issue{Check: "swag", Where: strip.SpecDir + "/" + name, Message: "regenerating gives different output: swag is not deterministic on these annotations"})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Where < out[j].Where })
	out = append(out, checkSpec(jsonData, in.Facts)...)
	return append(out, checkConstraints(jsonData, in.Facts)...)
}

// checkCode proves the code did not change: every .go file outside the
// generated spec package keeps its baseline code hash, go.mod is untouched
// and no go.sum appears.
func checkCode(base, cur guard.Snapshot) []Issue {
	var out []Issue
	for _, rel := range sortedKeys(cur.Files) {
		b, ok := base.Files[rel]
		if ok && strings.HasSuffix(rel, ".go") && !strings.HasPrefix(rel, strip.SpecDir+"/") && cur.Files[rel].CodeHash != b.CodeHash {
			out = append(out, Issue{Check: "code", Where: rel, Message: "the code changed, not only comments: only '// @' comment lines may be added"})
		}
	}
	if b, ok := base.Files["go.mod"]; ok && cur.Files["go.mod"] != b {
		out = append(out, Issue{Check: "code", Where: "go.mod", Message: "go.mod changed; the run may not add dependencies"})
	}
	if c, ok := cur.Files["go.sum"]; ok && c != base.Files["go.sum"] {
		out = append(out, Issue{Check: "code", Where: "go.sum", Message: "go.sum appeared or changed; the run may not add dependencies"})
	}
	return out
}

// checkScope proves that only .go comment lines and internal/apispec/
// changed: a changed .go file must equal its original once the annotation
// lines are stripped from both.
func checkScope(base, cur guard.Snapshot, dir string, original func(string) ([]byte, error)) []Issue {
	paths := map[string]bool{}
	for p := range base.Files {
		paths[p] = true
	}
	for p := range cur.Files {
		paths[p] = true
	}
	var out []Issue
	add := func(rel, msg string) { out = append(out, Issue{Check: "scope", Where: rel, Message: msg}) }
	for _, rel := range sortedKeys(paths) {
		b, inBase := base.Files[rel]
		c, inCur := cur.Files[rel]
		switch {
		case inBase && inCur && b.SHA256 == c.SHA256:
		case strings.HasPrefix(rel, strip.SpecDir+"/"):
		case rel == "go.mod" || rel == "go.sum": // reported by checkCode
		case !inCur:
			add(rel, "file was deleted")
		case !inBase:
			add(rel, "new file outside "+strip.SpecDir+"/")
		case !strings.HasSuffix(rel, ".go"):
			add(rel, "file changed; only '// @' lines in .go files and "+strip.SpecDir+"/ may change")
		case b.CodeHash != c.CodeHash: // reported by checkCode
		default:
			if msg := commentOnly(rel, dir, original); msg != "" {
				add(rel, msg)
			}
		}
	}
	return out
}

// commentOnly returns "" if rel differs from its original only in
// annotation lines, or what else changed.
func commentOnly(rel, dir string, original func(string) ([]byte, error)) string {
	orig, err := original(rel)
	if err != nil {
		return "cannot read the original: " + err.Error()
	}
	now, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
	if err != nil {
		return "cannot read the file: " + err.Error()
	}
	a, _, err1 := strip.Lines(rel, orig)
	b, _, err2 := strip.Lines(rel, now)
	if err1 != nil || err2 != nil {
		return "file does not scan as Go"
	}
	if bytes.Equal(a, b) {
		return ""
	}
	al, bl := strings.Split(string(a), "\n"), strings.Split(string(b), "\n")
	line := 1
	for line <= len(al) && line <= len(bl) && al[line-1] == bl[line-1] {
		line++
	}
	return fmt.Sprintf("lines other than '// @' annotations changed (first difference near line %d of the stripped file): existing comments and layout must stay as they are", line)
}
