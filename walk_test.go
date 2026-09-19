package sql_test

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/rqlite/sql"
)

type walkEvent struct {
	kind  string
	node  sql.Node
	depth int
}

type recordingVisitor struct {
	t      *testing.T
	events []walkEvent
	active []sql.Node
	seen   map[string]sql.Node
	errAt  sql.Node
	err    error
}

func newRecordingVisitor(t *testing.T, errAt sql.Node, err error) *recordingVisitor {
	t.Helper()
	return &recordingVisitor{
		t:     t,
		seen:  make(map[string]sql.Node),
		errAt: errAt,
		err:   err,
	}
}

func (v *recordingVisitor) Visit(node sql.Node) (sql.Visitor, sql.Node, error) {
	v.checkIdentity(node)
	v.events = append(v.events, walkEvent{kind: "enter", node: node, depth: len(v.active)})
	if v.errAt != nil && sameNode(node, v.errAt) {
		return nil, node, v.err
	}
	v.active = append(v.active, node)
	return v, node, nil
}

func (v *recordingVisitor) VisitEnd(node sql.Node) (sql.Node, error) {
	if len(v.active) == 0 || !sameNode(v.active[len(v.active)-1], node) {
		v.t.Fatalf("leave %T without matching enter; active stack=%v", node, v.active)
	}
	v.active = v.active[:len(v.active)-1]
	v.events = append(v.events, walkEvent{kind: "leave", node: node, depth: len(v.active)})
	return node, nil
}

func (v *recordingVisitor) checkIdentity(node sql.Node) {
	ptr, ok := nodePointer(node)
	if !ok {
		v.t.Fatalf("node %T does not have a stable pointer identity", node)
	}
	key := fmt.Sprintf("%T:%d", node, ptr)
	if previous := v.seen[key]; previous != nil {
		v.t.Fatalf("node %T visited twice: first=%s second=%s", node, previous, node)
	}
	v.seen[key] = node
}

func nodePointer(node sql.Node) (uintptr, bool) {
	value := reflect.ValueOf(node)
	if value.Kind() == reflect.Ptr {
		return value.Pointer(), true
	}
	if value.Kind() == reflect.Struct {
		field := value.Field(0)
		if field.Kind() == reflect.Ptr && !field.IsNil() {
			return field.Pointer(), true
		}
	}
	return 0, false
}

func sameNode(a, b sql.Node) bool {
	ap, aok := nodePointer(a)
	bp, bok := nodePointer(b)
	return aok && bok && ap == bp && reflect.TypeOf(a) == reflect.TypeOf(b)
}

func parseStatement(t *testing.T, query string) sql.Statement {
	t.Helper()
	stmt, err := sql.NewParser(strings.NewReader(query)).ParseStatement()
	if err != nil {
		t.Fatalf("ParseStatement(%q): %v", query, err)
	}
	return stmt
}

func assertWalkCompletesOnce(t *testing.T, stmt sql.Node) []walkEvent {
	t.Helper()
	visitor := newRecordingVisitor(t, nil, nil)
	returned, err := sql.Walk(visitor, stmt)
	if err != nil {
		t.Fatalf("Walk returned error: %v", err)
	}
	if returned != stmt {
		t.Fatalf("Walk returned %T, want original root %T", returned, stmt)
	}
	if len(visitor.active) != 0 {
		t.Fatalf("unbalanced visitor stack: %v", visitor.active)
	}
	if got, want := eventBalance(visitor.events), 0; got != want {
		t.Fatalf("event balance=%d, want %d", got, want)
	}
	return visitor.events
}

func eventBalance(events []walkEvent) int {
	balance := 0
	for _, event := range events {
		if event.kind == "enter" {
			balance++
		} else if event.kind == "leave" {
			balance--
		}
	}
	return balance
}

func findJoinClauses(events []walkEvent) []sql.Node {
	var nodes []sql.Node
	for _, event := range events {
		if event.kind == "enter" {
			if node, ok := event.node.(*sql.JoinClause); ok {
				nodes = append(nodes, node)
			}
		}
	}
	return nodes
}

func findParenSources(events []walkEvent) []sql.Node {
	var nodes []sql.Node
	for _, event := range events {
		if event.kind == "enter" {
			if node, ok := event.node.(*sql.ParenSource); ok {
				nodes = append(nodes, node)
			}
		}
	}
	return nodes
}

func TestWalkSourceContract(t *testing.T) {
	tests := []struct {
		name      string
		query     string
		source    []string
		fromDepth int
	}{
		{
			name:      "single table",
			query:     `UPDATE target SET value = 1 FROM source`,
			source:    []string{"source"},
			fromDepth: 1,
		},
		{
			name:      "aliased table",
			query:     `UPDATE target SET value = src.value FROM source AS src WHERE target.id = src.id`,
			source:    []string{"src"},
			fromDepth: 1,
		},
		{
			name:      "multi-level join",
			query:     `UPDATE target SET value = third.value FROM alpha JOIN beta ON alpha.id = beta.id JOIN gamma ON beta.id = gamma.id`,
			source:    []string{"join", "join", "alpha", "beta", "on", "gamma", "on"},
			fromDepth: 1,
		},
		{
			name:      "parenthesized source",
			query:     `UPDATE target SET value = src.value FROM (source) AS src`,
			source:    []string{"src", "source"},
			fromDepth: 1,
		},
		{
			name:      "uncorrelated subquery",
			query:     `UPDATE target SET value = src.value FROM (SELECT value FROM real_source) AS src`,
			source:    []string{"src", "select", "real_source"},
			fromDepth: 1,
		},
		{
			name:      "correlated subquery in source",
			query:     `UPDATE target SET value = src.value FROM (SELECT value FROM source WHERE source.id = target.id) AS src`,
			source:    []string{"src", "select", "source"},
			fromDepth: 1,
		},
		{
			name:   "correlated subquery in set",
			query:  `UPDATE target SET value = (SELECT max(source.value) FROM source WHERE source.id = target.id)`,
			source: nil,
		},
		{
			name:      "table-valued function",
			query:     `UPDATE target SET value = f.value FROM generate_series(1, target.bound) AS f`,
			source:    []string{"f"},
			fromDepth: 1,
		},
		{
			name:      "cte shadows real table",
			query:     `WITH source AS (SELECT value FROM backing) UPDATE target SET value = source.value FROM source JOIN real_table ON real_table.id = target.id`,
			source:    []string{"join", "source", "real_table", "on"},
			fromDepth: 1,
		},
		{
			name:   "no from",
			query:  `UPDATE target SET value = 1 WHERE id = 2`,
			source: nil,
		},
		{
			name:      "same name in set from and where",
			query:     `UPDATE orders SET total = source.total FROM source WHERE orders.id = source.id AND source.active = TRUE`,
			source:    []string{"source"},
			fromDepth: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stmt := parseStatement(t, tt.query)
			events := assertWalkCompletesOnce(t, stmt)

			if tt.source != nil {
				root := events[0].node.(*sql.UpdateStatement)
				assertUpdateChildOrder(t, events, root)
				sourceEvents := sourceEventsBetween(events, root.Source, root.WhereExpr)
				if got := sourceEventPath(sourceEvents); !reflect.DeepEqual(got, tt.source) {
					t.Fatalf("source path=%v, want %v; events:\n%s", got, tt.source, formatEvents(events))
				}
				if got := firstSourceDepth(sourceEvents); got != tt.fromDepth {
					t.Fatalf("source depth=%d, want %d", got, tt.fromDepth)
				}
				if hasNode(sourceEvents, root.Table) || hasNode(sourceEvents, root.WhereExpr) {
					t.Fatalf("source segment contained target or WHERE node:\n%s", formatEvents(sourceEvents))
				}
			} else if len(findJoinClauses(events)) != 0 || len(findParenSources(events)) != 0 {
				t.Fatalf("unexpected FROM source events:\n%s", formatEvents(events))
			}
		})
	}
}

func assertUpdateChildOrder(t *testing.T, events []walkEvent, update *sql.UpdateStatement) {
	t.Helper()
	wanted := []sql.Node{update.Table, update.Assignments[0], update.Source, update.WhereExpr}
	children := wanted[:0]
	for _, node := range wanted {
		if node != nil {
			children = append(children, node)
		}
	}
	next := 0
	for _, event := range events {
		if event.kind == "enter" && sameNode(event.node, children[next]) {
			next++
			if next == len(children) {
				return
			}
		}
	}
	t.Fatalf("UPDATE child order missing %T:\n%s", children[next], formatEvents(events))
}

func sourceEventsBetween(events []walkEvent, source sql.Node, where sql.Node) []walkEvent {
	sourcePtr, _ := nodePointer(source)
	wherePtr, _ := nodePointer(where)
	var result []walkEvent
	inSource := false
	for _, event := range events {
		ptr, _ := nodePointer(event.node)
		if event.kind == "enter" && ptr == sourcePtr {
			inSource = true
		}
		if inSource {
			result = append(result, event)
		}
		if event.kind == "leave" && ptr == sourcePtr {
			inSource = false
		}
		if event.kind == "enter" && ptr == wherePtr {
			break
		}
	}
	return result
}

func firstSourceDepth(events []walkEvent) int {
	for _, event := range events {
		if event.kind == "enter" {
			return event.depth
		}
	}
	return -1
}

func hasNode(events []walkEvent, wanted sql.Node) bool {
	ptr, _ := nodePointer(wanted)
	for _, event := range events {
		p, _ := nodePointer(event.node)
		if p == ptr {
			return true
		}
	}
	return false
}

func sourceEventPath(events []walkEvent) []string {
	path := make([]string, 0, len(events))
	for _, event := range events {
		if event.kind != "enter" {
			continue
		}
		switch node := event.node.(type) {
		case *sql.QualifiedTableName:
			if node.Alias != nil {
				path = append(path, node.Alias.Name)
			} else {
				path = append(path, node.Name.Name)
			}
		case *sql.QualifiedTableFunctionName:
			if node.Alias != nil {
				path = append(path, node.Alias.Name)
			} else {
				path = append(path, node.Name.Name)
			}
		case *sql.ParenSource:
			path = append(path, node.Alias.Name)
		case *sql.JoinClause:
			path = append(path, "join")
		case *sql.SelectStatement:
			path = append(path, "select")
		case *sql.OnConstraint:
			path = append(path, "on")
		}
	}
	return path
}

func formatEvents(events []walkEvent) string {
	lines := make([]string, 0, len(events))
	for _, event := range events {
		lines = append(lines, fmt.Sprintf("%s%d %T %s", strings.Repeat("  ", event.depth), event.depth, event.node, event.node))
		prefix := "enter "
		if event.kind == "leave" {
			prefix = "leave "
		}
		lines[len(lines)-1] = prefix + lines[len(lines)-1]
	}
	return strings.Join(lines, "\n")
}

type tenantRewriter struct {
	parents []sql.Node
	scopes  [][]string
	stopAt  sql.Node
	err     error
}

func (r *tenantRewriter) Visit(node sql.Node) (sql.Visitor, sql.Node, error) {
	if r.stopAt != nil && sameNode(node, r.stopAt) {
		return nil, node, r.err
	}
	r.parents = append(r.parents, node)

	switch node := node.(type) {
	case *sql.WithClause:
		r.scopes = append(r.scopes, nil)
	case *sql.CTE:
		if withClause, ok := r.parent().(*sql.WithClause); ok && withClause.Recursive.IsValid() {
			r.scopes = append(r.scopes, []string{node.TableName.Name})
		}
	case *sql.Ident:
		if parent, ok := r.parent().(*sql.QualifiedTableName); ok && parent.Name == node {
			if !r.isCTEName(node.Name) {
				node.Name = "tenant_" + node.Name
			}
		}
	}
	return r, node, nil
}

func (r *tenantRewriter) VisitEnd(node sql.Node) (sql.Node, error) {
	if cte, ok := node.(*sql.CTE); ok && len(r.scopes) > 0 {
		if withClause, ok := r.parent().(*sql.WithClause); ok && withClause.Recursive.IsValid() {
			r.scopes = r.scopes[:len(r.scopes)-1]
		}
		r.scopes[len(r.scopes)-1] = append(r.scopes[len(r.scopes)-1], cte.TableName.Name)
	}
	switch node.(type) {
	case *sql.SelectStatement, *sql.InsertStatement, *sql.UpdateStatement, *sql.DeleteStatement:
		if withClause := statementWithClause(node); withClause != nil && len(r.scopes) > 0 {
			r.scopes = r.scopes[:len(r.scopes)-1]
		}
	}
	return node, nil
}

func statementWithClause(node sql.Node) *sql.WithClause {
	switch node := node.(type) {
	case *sql.SelectStatement:
		return node.WithClause
	case *sql.InsertStatement:
		return node.WithClause
	case *sql.UpdateStatement:
		return node.WithClause
	case *sql.DeleteStatement:
		return node.WithClause
	default:
		return nil
	}
}

func (r *tenantRewriter) parent() sql.Node {
	if len(r.parents) < 2 {
		return nil
	}
	return r.parents[len(r.parents)-2]
}

func (r *tenantRewriter) isCTEName(name string) bool {
	for i := len(r.scopes) - 1; i >= 0; i-- {
		for _, cteName := range r.scopes[i] {
			if cteName == name {
				return true
			}
		}
	}
	return false
}

func TestWalkTenantIdentifierRewrite(t *testing.T) {
	query := `WITH source AS (SELECT value FROM backing) UPDATE target AS t SET total = source.total, marker = 'source' FROM source AS s JOIN real AS r ON r.id = t.id WHERE t.id = s.id`
	stmt := parseStatement(t, query).(*sql.UpdateStatement)

	rewritten, err := sql.Walk(&tenantRewriter{}, stmt)
	if err != nil {
		t.Fatalf("Walk returned error: %v", err)
	}
	update := rewritten.(*sql.UpdateStatement)
	formatted := update.String()
	reparsed := parseStatement(t, formatted).(*sql.UpdateStatement)

	if got := reparsed.WithClause.CTEs[0].TableName.Name; got != "source" {
		t.Fatalf("CTE name=%q, want source", got)
	}
	backing := reparsed.WithClause.CTEs[0].Select.Source.(*sql.QualifiedTableName)
	if backing.Name.Name != "tenant_backing" {
		t.Fatalf("CTE backing table=%q, want tenant_backing", backing.Name.Name)
	}
	if reparsed.Table.Name.Name != "tenant_target" || reparsed.Table.Alias.Name != "t" {
		t.Fatalf("target=(%q AS %q), want tenant_target AS t", reparsed.Table.Name.Name, reparsed.Table.Alias.Name)
	}
	join := reparsed.Source.(*sql.JoinClause)
	cteSource := join.X.(*sql.QualifiedTableName)
	if cteSource.Name.Name != "source" || cteSource.Alias.Name != "s" {
		t.Fatalf("CTE source=(%q AS %q), want source AS s", cteSource.Name.Name, cteSource.Alias.Name)
	}
	realTable := join.Y.(*sql.QualifiedTableName)
	if realTable.Name.Name != "tenant_real" || realTable.Alias.Name != "r" {
		t.Fatalf("real source=(%q AS %q), want tenant_real AS r", realTable.Name.Name, realTable.Alias.Name)
	}
	on := join.Constraint.(*sql.OnConstraint).X.(*sql.BinaryExpr)
	if on.X.(*sql.QualifiedRef).Table.Name != "r" || on.Y.(*sql.QualifiedRef).Table.Name != "t" {
		t.Fatalf("join qualifiers=(%s, %s), want r and t", on.X, on.Y)
	}
	total := reparsed.Assignments[0].Expr.(*sql.QualifiedRef)
	if total.Table.Name != "source" {
		t.Fatalf("SET qualifier=%q, want source", total.Table.Name)
	}
	if reparsed.Assignments[1].Expr.(*sql.StringLit).Value != "source" {
		t.Fatalf("string literal changed to %q", reparsed.Assignments[1].Expr)
	}
	where := reparsed.WhereExpr.(*sql.BinaryExpr)
	if where.X.(*sql.QualifiedRef).Table.Name != "t" || where.Y.(*sql.QualifiedRef).Table.Name != "s" {
		t.Fatalf("WHERE qualifiers=(%s, %s), want t and s", where.X, where.Y)
	}
	if strings.Contains(formatted, "tenant_tenant_") {
		t.Fatalf("formatted SQL contains a double tenant prefix: %s", formatted)
	}
	if clone := update.Clone(); clone.String() != formatted {
		t.Fatalf("Clone/String changed output:\nclone=%s\noriginal=%s", clone, formatted)
	}
}

func TestWalkSourceShortCircuit(t *testing.T) {
	queries := map[string]string{
		"shallow":          `UPDATE target SET value = 1 FROM shallow`,
		"deep nested join": `UPDATE target SET value = gamma.value FROM alpha JOIN beta ON alpha.id = beta.id JOIN gamma ON beta.id = gamma.id`,
		"nested subquery":  `UPDATE target SET value = src.value FROM (SELECT value FROM backing) AS src`,
	}

	for name, query := range queries {
		t.Run(name, func(t *testing.T) {
			stmt := parseStatement(t, query)
			target := findErrorTarget(t, stmt)
			wantErr := errors.New("source subtree stopped")
			visitor := &tenantRewriter{stopAt: target, err: wantErr}

			returned, err := sql.Walk(visitor, stmt)
			if !errors.Is(err, wantErr) || err != wantErr {
				t.Fatalf("error=%v, want exact error %v", err, wantErr)
			}
			if returned != nil {
				t.Fatalf("returned node=%v, want nil after error", returned)
			}
			assertSourceShortCircuit(t, stmt, target, wantErr)
			assertNoDoubleTenantPrefix(t, stmt)
		})
	}
}

func assertSourceShortCircuit(t *testing.T, stmt sql.Node, target sql.Node, wantErr error) {
	t.Helper()
	recorder := newRecordingVisitor(t, target, wantErr)
	returned, err := sql.Walk(recorder, stmt)
	if err != wantErr || returned != nil {
		t.Fatalf("recording Walk returned (%v, %v), want (%v, nil)", err, returned, wantErr)
	}
	last := recorder.events[len(recorder.events)-1]
	if last.kind != "enter" || !sameNode(last.node, target) {
		t.Fatalf("last event=%v %T, want enter target %T", last.kind, last.node, target)
	}
	leaveCount := 0
	for _, event := range recorder.events {
		if event.kind == "leave" {
			leaveCount++
		}
	}
	enteredBeforeTarget := countEntersBeforeTarget(recorder.events, target)
	if wantLeaves := enteredBeforeTarget - len(recorder.active); leaveCount != wantLeaves {
		t.Fatalf("leave count=%d, want %d (%d entered before target with %d still active):\n%s", leaveCount, wantLeaves, enteredBeforeTarget, len(recorder.active), formatEvents(recorder.events))
	}
}

func findErrorTarget(t *testing.T, stmt sql.Node) sql.Node {
	t.Helper()
	var target sql.Node
	_, err := sql.Walk(sql.VisitFunc(func(node sql.Node) (sql.Node, error) {
		if target == nil {
			switch node := node.(type) {
			case *sql.QualifiedTableName:
				if node.Name.Name == "shallow" || node.Name.Name == "gamma" || node.Name.Name == "backing" {
					target = node
				}
			}
		}
		return node, nil
	}), stmt)
	if err != nil {
		t.Fatalf("target discovery failed: %v", err)
	}
	if target == nil {
		t.Fatal("did not find error target")
	}
	return target
}

func countEntersBeforeTarget(events []walkEvent, target sql.Node) int {
	count := 0
	for _, event := range events {
		if event.kind == "enter" && sameNode(event.node, target) {
			break
		}
		if event.kind == "enter" {
			count++
		}
	}
	return count
}

func assertNoDoubleTenantPrefix(t *testing.T, node sql.Node) {
	t.Helper()
	_, err := sql.Walk(sql.VisitFunc(func(node sql.Node) (sql.Node, error) {
		if ident, ok := node.(*sql.Ident); ok && strings.Contains(ident.Name, "tenant_tenant_") {
			t.Fatalf("identifier %q contains a repeated rewrite prefix", ident.Name)
		}
		return node, nil
	}), node)
	if err != nil {
		t.Fatalf("inspection walk failed: %v", err)
	}
}

func TestWalkSelectSourceOrder(t *testing.T) {
	query := `SELECT target.id FROM source WHERE target.id = source.id`
	events := assertWalkCompletesOnce(t, parseStatement(t, query))
	var kinds []string
	for _, event := range events {
		if event.kind != "enter" {
			continue
		}
		switch node := event.node.(type) {
		case *sql.ResultColumn:
			kinds = append(kinds, "columns")
		case *sql.QualifiedTableName:
			if node.Name.Name == "source" {
				kinds = append(kinds, "source")
			}
		case *sql.BinaryExpr:
			kinds = append(kinds, "where")
		}
	}
	if got := strings.Join(kinds, ","); got != "columns,source,where" {
		t.Fatalf("SELECT child order=%q, want columns,source,where", got)
	}
}

func TestWalkSourceMutationsFail(t *testing.T) {
	if os.Getenv("WALK_SOURCE_MUTATION") != "" {
		t.Skip("mutation child process")
	}

	mutations := []struct {
		name   string
		mutate func(string) string
	}{
		{
			name: "source recursion removed",
			mutate: func(source string) string {
				return strings.Replace(source, `if source, err := walkSource(v, nn.Source); err != nil {
			return nil, err
		} else {
			nn.Source = source
		}`, `_ = nn.Source`, -1)
			},
		},
		{
			name: "source visited after where",
			mutate: func(source string) string {
				block := `if source, err := walkSource(v, nn.Source); err != nil {
			return nil, err
		} else {
			nn.Source = source
		}`
				marker := `if expr, err := walkExpr(v, nn.WhereExpr); err != nil {
			return nil, err
		} else {
			nn.WhereExpr = expr
		}`
				moveInCase := func(text, statementCase, nextCase string) string {
					start := strings.Index(text, "case *"+statementCase+":")
					if start < 0 {
						t.Fatalf("missing %s case", statementCase)
					}
					next := strings.Index(text[start:], "case *"+nextCase+":")
					if next < 0 {
						t.Fatalf("missing %s case", nextCase)
					}
					end := start + next
					section := text[start:end]
					section = strings.Replace(section, block, "", 1)
					section = strings.Replace(section, marker, marker+"\n\t\t"+block, 1)
					return text[:start] + section + text[end:]
				}
				mutated := moveInCase(source, "SelectStatement", "InsertStatement")
				mutated = moveInCase(mutated, "UpdateStatement", "UpsertClause")
				return mutated
			},
		},
		{
			name: "source errors ignored",
			mutate: func(source string) string {
				return strings.Replace(source, `func walkSource(v Visitor, source Source) (Source, error) {
	if source == nil {
		return nil, nil
	}
	rn, err := walk(v, source)
	if err != nil {
		return nil, err
	}
	return rn.(Source), nil
}`, `func walkSource(v Visitor, source Source) (Source, error) {
	if source == nil {
		return nil, nil
	}
	rn, _ := walk(v, source)
	if rn == nil {
		return source, nil
	}
	return rn.(Source), nil
}`, 1)
			},
		},
	}

	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			dir := t.TempDir()
			copyMutationFiles(t, dir)

			walkPath := filepath.Join(dir, "walk.go")
			original, err := os.ReadFile(walkPath)
			if err != nil {
				t.Fatal(err)
			}
			mutated := mutation.mutate(string(original))
			if mutated == string(original) {
				t.Fatal("mutation did not change walk.go")
			}
			if err := os.WriteFile(walkPath, []byte(mutated), 0o644); err != nil {
				t.Fatal(err)
			}

			cmd := exec.Command("go", "test", ".", "-run", "TestWalkMutationProbe$", "-count=1")
			cmd.Dir = dir
			cmd.Env = append(os.Environ(), "WALK_SOURCE_MUTATION="+mutation.name)
			output, err := cmd.CombinedOutput()
			if err == nil {
				t.Fatalf("mutation test unexpectedly passed\n%s", output)
			}
			if exitErr, ok := err.(*exec.ExitError); !ok || exitErr.ExitCode() == 0 {
				t.Fatalf("mutation failed without a non-zero test exit: %v\n%s", err, output)
			}
		})
	}
}

func copyMutationFiles(t *testing.T, dir string) {
	t.Helper()
	matches, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range matches {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module github.com/rqlite/sql\n\ngo 1.17\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	probe, err := os.ReadFile(filepath.Join("testdata", "walk_mutation_probe.go.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "walk_mutation_probe_test.go"), probe, 0o644); err != nil {
		t.Fatal(err)
	}
}
