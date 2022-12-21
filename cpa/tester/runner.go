package tester

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/CircleCI-Public/circle-policy-agent/cpa"
	"github.com/CircleCI-Public/circle-policy-agent/internal"
	"github.com/open-policy-agent/opa/tester"
	"github.com/yazgazan/jaydiff/diff"
	"golang.org/x/exp/slices"
)

type Runner struct {
	include *regexp.Regexp
	folders []string
}

type RunnerOptions struct {
	Path    string
	Include *regexp.Regexp
}

var ErrNoTests = errors.New("no tests")

func NewRunner(opts RunnerOptions) (*Runner, error) {
	if opts.Path == "" {
		opts.Path = "./..."
	}

	folders, err := getTestFolders(opts.Path)
	if err != nil {
		return nil, fmt.Errorf("failed to lookup test folders: %w", err)
	}

	return &Runner{folders: folders, include: opts.Include}, nil
}

func (runner *Runner) Run() <-chan Result {
	results := make(chan Result)

	go func() {
		runner.runOpaTests(results)
		for _, folder := range runner.folders {
			runner.runFolder(folder, results)
		}
		close(results)
	}()

	return results
}

func (runner *Runner) RunAndHandleResults(handler ResultHandler) bool {
	return handler.HandleResults(runner.Run())
}

func (runner *Runner) runOpaTests(results chan<- Result) {
	root := runner.folders[0]

	policy, err := cpa.LoadPolicyFromFS(root)
	if err != nil {
		if errors.Is(err, cpa.ErrNoPolicies) {
			return
		}
		results <- Result{
			Group: "<opa.tests>",
			Err:   err,
		}
		return
	}

	for r := range internal.Must(tester.NewRunner().Run(context.Background(), policy.Modules())) {
		name := r.Package + "." + r.Name
		if runner.include != nil && !runner.include.MatchString(name) {
			continue
		}
		results <- Result{
			Group:   "<opa.tests>",
			Name:    name,
			Passed:  r.Pass(),
			Elapsed: r.Duration,
			Err:     r.Error,
		}
	}
}

func (runner *Runner) runFolder(folder string, results chan<- Result) {
	policy, err := cpa.LoadPolicyFromFS(folder)
	if err != nil {
		results <- Result{
			Group:  folder,
			Err:    err,
			Passed: errors.Is(err, cpa.ErrNoPolicies),
		}
		return
	}

	nameSet := map[string]struct{}{}
	var namedTests []NamedTest

	entries, err := os.ReadDir(folder)
	if err != nil {
		results <- Result{
			Group: folder,
			Err:   err,
		}
		return
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if name := entry.Name(); !strings.HasSuffix(name, "_test.yaml") && !strings.HasSuffix(name, "_test.yml") {
			continue
		}

		testPath := filepath.Join(folder, entry.Name())

		tests, err := loadTests(testPath)
		if err != nil {
			results <- Result{
				Group: folder,
				Err:   err,
			}
			return
		}

		for name, test := range tests {
			if _, ok := nameSet[name]; ok {
				results <- Result{
					Group: folder,
					Err:   fmt.Errorf("test name conflict: %q", name),
				}
				return
			}
			nameSet[name] = struct{}{}
			namedTests = append(namedTests, NamedTest{name, test})
		}
	}

	if len(namedTests) == 0 {
		results <- Result{
			Group:  folder,
			Passed: true,
			Err:    ErrNoTests,
		}
		return
	}

	slices.SortFunc(namedTests, func(a, b NamedTest) bool { return a.Name < b.Name })

	for _, t := range namedTests {
		runner.runTest(policy, results, t, folder, ParentTestContext{})
	}
}

func (runner *Runner) runTest(policy *cpa.Policy, results chan<- Result, t NamedTest, group string, parent ParentTestContext) {
	input := func() any {
		if t.Input == nil {
			return parent.Input
		}
		return internal.Merge(parent.Input, t.Input)
	}()

	meta := func() any {
		if t.Meta == nil {
			return parent.Meta
		}
		return internal.Merge(parent.Meta, t.Meta)
	}()

	name := t.Name
	if parent.Name != "" {
		name = parent.Name + "/" + name
	}

	if runner.include == nil || runner.include.MatchString(name) {
		eval, _ := policy.Eval(context.Background(), "data", input, cpa.Meta(meta))

		start := time.Now()
		var decision any = internal.Must(policy.Decide(context.Background(), input, cpa.Meta(meta)))
		elapsed := time.Since(start)

		decision = internal.Must(internal.ToRawInterface(decision))

		d := internal.Must(diff.Diff(decision, t.Decision))

		results <- Result{
			Group:  group,
			Name:   name,
			Passed: d.Diff() == diff.Identical,
			Err: func() error {
				if d.Diff() == diff.Identical {
					return nil
				}
				return errors.New(d.StringIndent("", "  ", diff.Output{
					Indent:     "  ",
					Colorized:  true,
					JSON:       true,
					JSONValues: true,
				}))
			}(),
			Elapsed: elapsed,
			Ctx: map[string]any{
				"input":      input,
				"meta":       meta,
				"decision":   decision,
				"evaluation": eval,
			},
		}
	}

	for _, subtest := range t.NamedCases() {
		runner.runTest(policy, results, subtest, group, ParentTestContext{
			Name:  name,
			Input: input,
			Meta:  meta,
		})
	}
}