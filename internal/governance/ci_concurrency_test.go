package governance

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// repoRoot walks up from the test's working directory to the module root.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found above the test directory")
		}
		dir = parent
	}
}

// workflowEvents returns the names a workflow is triggered by. `on:` is either
// one event, a list of them, or a mapping whose keys carry each event options.
func workflowEvents(doc map[string]any) map[string]bool {
	out := map[string]bool{}
	switch on := doc["on"].(type) {
	case string:
		out[strings.ToLower(on)] = true
	case []any:
		for _, event := range on {
			out[strings.ToLower(fmt.Sprint(event))] = true
		}
	case map[string]any:
		for name := range on {
			out[strings.ToLower(name)] = true
		}
	}
	return out
}

// concurrencyGroup returns the run-level concurrency group expression, and
// whether the workflow declares run-level concurrency at all. `concurrency:` is
// either that expression on its own or a mapping holding it.
func concurrencyGroup(doc map[string]any) (string, bool) {
	switch c := doc["concurrency"].(type) {
	case nil:
		return "", false
	case string:
		return c, true
	case map[string]any:
		group, _ := c["group"].(string)
		return group, true
	default:
		return fmt.Sprint(c), true
	}
}

// TestWorkflowConcurrencySeparatesPRs pins the concurrency key of every Gitea
// workflow in this repository.
//
// On this act_runner a pull_request run carries no ref (every pull_request row
// in actions/runs has a null ref and head_branch, while the push rows show
// main and train/* correctly), so the ref-only key collapsed all open PRs into
// one group and a push to any one cancelled the runs already in flight on the
// others: run 18781 ended `cancelled` with no failing job, its statuses read
// `failure`, and the merge gate blocked on an otherwise healthy tree (#482).
// The per-PR term keeps the group correct whichever way the runner fills ref.
//
// The ref still belongs in the key: main, the train/* branches and tags each
// need their own group, or a push to main would cancel the run already
// shipping it.
func TestWorkflowConcurrencySeparatesPRs(t *testing.T) {
	files, err := filepath.Glob(filepath.Join(repoRoot(t), ".gitea", "workflows", "*.y*ml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no workflows found under .gitea/workflows")
	}

	for _, path := range files {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var doc map[string]any
		if err := yaml.Unmarshal(body, &doc); err != nil {
			t.Errorf("%s: parse workflow: %v", filepath.Base(path), err)
			continue
		}
		group, declared := concurrencyGroup(doc)
		if !declared {
			continue
		}
		name := filepath.Base(path)
		triggers := workflowEvents(doc)

		if !strings.Contains(group, ".ref") && (triggers["push"] || triggers["pull_request"]) {
			t.Errorf("%s: concurrency group %q separates nothing: push runs of main and the train share one group", name, group)
		}
		if triggers["pull_request"] && !strings.Contains(group, "pull_request.number") {
			t.Errorf("%s: concurrency group %q has no per-PR term; append ${{ gitea.event.pull_request.number }} so each pull request keeps its own group regardless of how the runner fills ref (#482)", name, group)
		}
	}
}
