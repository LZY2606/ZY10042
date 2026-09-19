package sql_test

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/go-test/deep"
	"github.com/rqlite/sql"
)

type walkEvent struct {
	enter bool
	path  string
	kind  string
	node  sql.Node
}

type strictVisitor struct {
	t      *testing.T
	frames []walkFrame
	events []walkEvent
	seen   map[string]int
}

type walkFrame struct {
	path string
	node sql.Node
}

func newStrictVisitor(t *testing.T) *strictVisitor {
	return &strictVisitor{t: t, seen: make(map[string]int)}
}

func (v *strictVisitor) Visit(n sql.Node) (sql.Visitor, sql.Node, error) {
	path := typeName(n)
	if len(v.frames) > 0 {
		path = v.frames[len(v.frames)-1].path + "." + childPath(v.frames[len(v.frames)-1].node, n)
	}
	identity := nodeIdentity(n)
	if count := v.seen[identity]; count > 0 {
		v.t.Fatalf("node visited more than once: %s (%T) count=%d identity=%s", path, n, count+1, identity)
	}
	v.seen[identity]++
	v.frames = append(v.frames, walkFrame{path: path, node: n})
	v.events = append(v.events, walkEvent{enter: true, path: path, kind: typeName(n), node: n})
	return v, n, nil
}

func (v *strictVisitor) VisitEnd(n sql.Node) (sql.Node, error) {
	if len(v.frames) == 0 {
		v.t.Fatalf("unexpected leave for %T", n)
	}
	frame := v.frames[len(v.frames)-1]
	if frame.node != n {
		v.t.Fatalf("leave %T at %s, expected %T", n, frame.path, frame.node)
	}
	v.events = append(v.events, walkEvent{enter: false, path: frame.path, kind: typeName(n), node: n})
	v.frames = v.frames[:len(v.frames)-1]
	return n, nil
}

func (v *strictVisitor) paths(enter bool) []string {
	var paths []string
	for _, event := range v.events {
		if event.enter == enter {
			paths = append(paths, event.path)
		}
	}
	return paths
}

func (v *strictVisitor) assertComplete(root sql.Node) {
	v.t.Helper()
	expected := expectedEventPaths(root)
	got := make([]string, 0, len(v.events))
	for _, event := range v.events {
		got = append(got, event.path)
	}
	if diff := deep.Equal(got, expected); diff != nil {
		v.t.Fatalf("Walk event sequence mismatch:\n%s", strings.Join(diff, "\n"))
	}
	for identity, count := range v.seen {
		if count != 1 {
			v.t.Fatalf("node %s visited %d times", identity, count)
		}
	}
	if len(v.frames) != 0 {
		v.t.Fatalf("unbalanced traversal, frames=%d", len(v.frames))
	}
}

func expectedEventPaths(root sql.Node) []string {
	var events []string
	var walkExpected func(sql.Node, string)
	walkExpected = func(n sql.Node, path string) {
		kind := typeName(n)
		full := path + kind
		events = append(events, full)
		value := reflect.ValueOf(n)
		if value.Kind() == reflect.Ptr {
			value = value.Elem()
		}
		for i := 0; i < value.NumField(); i++ {
			field := value.Field(i)
			base := full + "." + value.Type().Field(i).Name
			switch field.Kind() {
			case reflect.Ptr, reflect.Interface:
				if child, ok := nodeValue(field); ok {
					walkExpected(child, base+".")
				}
			case reflect.Slice:
				for j := 0; j < field.Len(); j++ {
					if child, ok := nodeValue(field.Index(j)); ok {
						walkExpected(child, fmt.Sprintf("%s[%d].", base, j))
					}
				}
			}
		}
		events = append(events, full)
	}
	walkExpected(root, "")
	return events
}

func nodeValue(value reflect.Value) (sql.Node, bool) {
	if !value.IsValid() || value.Kind() != reflect.Ptr && value.Kind() != reflect.Interface {
		return nil, false
	}
	if value.IsNil() {
		return nil, false
	}
	if value.Kind() == reflect.Interface {
		if n, ok := value.Interface().(sql.Node); ok {
			return n, true
		}
		return nil, false
	}
	if value.Kind() == reflect.Ptr {
		if n, ok := value.Interface().(sql.Node); ok {
			return n, true
		}
	}
	return nil, false
}

func childPath(parent, child sql.Node) string {
	value := reflect.ValueOf(parent)
	if value.Kind() == reflect.Ptr {
		value = value.Elem()
	}
	for i := 0; i < value.NumField(); i++ {
		field := value.Field(i)
		if field.Kind() != reflect.Ptr && field.Kind() != reflect.Interface && field.Kind() != reflect.Slice {
			continue
		}
		if field.Kind() == reflect.Slice {
			for j := 0; j < field.Len(); j++ {
				if n, ok := nodeValue(field.Index(j)); ok && n == child {
					return fmt.Sprintf("%s[%d].%s", value.Type().Field(i).Name, j, typeName(child))
				}
			}
			continue
		}
		if n, ok := nodeValue(field); ok && n == child {
			return value.Type().Field(i).Name + "." + typeName(child)
		}
	}
	panic(fmt.Sprintf("child %T not found in parent %T", child, parent))
}

func typeName(n sql.Node) string {
	return strings.TrimPrefix(fmt.Sprintf("%T", n), "*sql.")
}

func nodeIdentity(n sql.Node) string {
	value := reflect.ValueOf(n)
	if value.Kind() == reflect.Ptr {
		return typeName(n) + ":" + fmt.Sprintf("%#x", value.Pointer())
	}
	if selectExpr, ok := n.(sql.SelectExpr); ok {
		return "SelectExpr:" + fmt.Sprintf("%#x", reflect.ValueOf(selectExpr.SelectStatement).Pointer())
	}
	panic(fmt.Sprintf("unsupported non-pointer node type %T", n))
}

func parseStatement(t *testing.T, query string) sql.Statement {
	t.Helper()
	stmt, err := sql.NewParser(strings.NewReader(query)).ParseStatement()
	if err != nil {
		t.Fatalf("ParseStatement(%q): %v", query, err)
	}
	return stmt
}

func walkStrict(t *testing.T, query string) (*strictVisitor, sql.Statement) {
	t.Helper()
	stmt := parseStatement(t, query)
	visitor := newStrictVisitor(t)
	root, err := sql.Walk(visitor, stmt)
	if err != nil {
		t.Fatalf("Walk(%q): %v", query, err)
	}
	if root != stmt {
		t.Fatal("Walk returned a different root without rewriting")
	}
	visitor.assertComplete(stmt)
	return visitor, stmt
}

func indexOfPath(paths []string, path string) int {
	for i, candidate := range paths {
		if candidate == path {
			return i
		}
	}
	return -1
}

func assertOrder(t *testing.T, paths []string, names ...string) {
	t.Helper()
	last := -1
	for _, name := range names {
		index := indexOfPath(paths, name)
		if index < 0 {
			t.Fatalf("missing expected path %s", name)
		}
		if index <= last {
			t.Fatalf("path %s index=%d must be after index=%d", name, index, last)
		}
		last = index
	}
}

func TestWalkSources(t *testing.T) {
	tests := []struct {
		name  string
		query string
	}{
		{
			name:  "single table",
			query: `UPDATE target SET value = source.value FROM source WHERE target.id = source.id`,
		},
		{
			name:  "aliases",
			query: `UPDATE target AS t SET value = s.value FROM source AS s WHERE t.id = s.id`,
		},
		{
			name:  "multi-level joins",
			query: `UPDATE target AS t SET value = s.value FROM source AS s JOIN join_one AS j1 ON s.id = j1.id LEFT JOIN join_two AS j2 ON j1.id = j2.id CROSS JOIN join_three AS j3 WHERE t.id = s.id`,
		},
		{
			name:  "parenthesized source",
			query: `UPDATE target AS t SET value = s.value FROM (source AS s CROSS JOIN join_source AS j) WHERE t.id = s.id`,
		},
		{
			name:  "uncorrelated subquery",
			query: `UPDATE target AS t SET value = s.value FROM (SELECT id, value FROM source) AS s WHERE t.id = s.id`,
		},
		{
			name:  "correlated subquery",
			query: `UPDATE target AS t SET value = s.value FROM source AS s WHERE t.id = s.id AND EXISTS (SELECT 1 FROM related AS r WHERE r.id = t.id)`,
		},
		{
			name:  "table-valued function",
			query: `UPDATE target AS t SET value = s.value FROM source_table(?, 42) AS s WHERE t.id = s.id`,
		},
		{
			name:  "cte shadows real table",
			query: `WITH source AS (SELECT id FROM real_source) UPDATE target AS t SET value = s.value FROM source AS s WHERE t.id = s.id`,
		},
		{
			name:  "no from",
			query: `UPDATE target AS t SET value = ? WHERE t.id = ?`,
		},
		{
			name:  "same name in set from where",
			query: `UPDATE same AS t SET value = s.value FROM same AS s WHERE t.id = s.id AND s.value = ?`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			visitor, _ := walkStrict(t, tt.query)
			paths := visitor.paths(true)
			assertOrder(t, paths,
				"UpdateStatement.Table.QualifiedTableName",
				"UpdateStatement.Assignments[0].Assignment",
			)
			if tt.name != "no from" {
				sourcePath := "UpdateStatement.Source." + firstSourceKind(t, paths)
				assertOrder(t, paths,
					"UpdateStatement.Assignments[0].Assignment",
					sourcePath,
					"UpdateStatement.WhereExpr.BinaryExpr",
				)
			}
		})
	}
}

func firstSourceKind(t *testing.T, paths []string) string {
	t.Helper()
	const prefix = "UpdateStatement.Source."
	for _, path := range paths {
		if strings.HasPrefix(path, prefix) {
			rest := strings.TrimPrefix(path, prefix)
			return strings.SplitN(rest, ".", 2)[0]
		}
	}
	t.Fatal("UPDATE statement has no source path")
	return ""
}

func TestWalkSelectSourceOrder(t *testing.T) {
	visitor, _ := walkStrict(t, `SELECT s.value FROM source AS s WHERE s.id = ?`)
	assertOrder(t, visitor.paths(true),
		"SelectStatement.Columns[0].ResultColumn",
		"SelectStatement.Source.QualifiedTableName",
		"SelectStatement.WhereExpr.BinaryExpr",
	)
}

func TestWalkInsertOrderUnchanged(t *testing.T) {
	visitor, _ := walkStrict(t, `INSERT INTO target (value) SELECT value FROM source`)
	assertOrder(t, visitor.paths(true),
		"InsertStatement.Table.Ident",
		"InsertStatement.Columns[0].Ident",
		"InsertStatement.Select.SelectStatement.Source.QualifiedTableName",
	)
}

func TestWalkSelectExprRewrite(t *testing.T) {
	stmt := parseStatement(t, `SELECT (SELECT id FROM source)`)
	root, err := sql.Walk(sql.VisitFunc(func(node sql.Node) (sql.Node, error) {
		if table, ok := node.(*sql.QualifiedTableName); ok && table.Name.Name == "source" {
			table.Name = table.Name.Clone()
			table.Name.Name = "rewritten_source"
		}
		return node, nil
	}), stmt)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := root.String(), `SELECT (SELECT "id" FROM "rewritten_source")`; got != want {
		t.Fatalf("Walk() = %q, want %q", got, want)
	}
}

type tenantVisitor struct {
	stack []tenantFrame
}

type tenantFrame struct {
	ctes     map[string]struct{}
	defining *sql.CTE
}

func newTenantVisitor() *tenantVisitor {
	return &tenantVisitor{}
}

func (v *tenantVisitor) Visit(n sql.Node) (sql.Visitor, sql.Node, error) {
	frame := tenantFrame{ctes: map[string]struct{}{}}
	if len(v.stack) > 0 {
		for name := range v.stack[len(v.stack)-1].ctes {
			frame.ctes[name] = struct{}{}
		}
	}
	switch n := n.(type) {
	case *sql.CTE:
		frame.defining = n
	case *sql.QualifiedTableName:
		if n.Schema == nil && !v.isCTE(n.Name) {
			n.Name = n.Name.Clone()
			n.Name.Name = "tenant_" + n.Name.Name
		}
	}
	v.stack = append(v.stack, frame)
	return v, n, nil
}

func (v *tenantVisitor) VisitEnd(n sql.Node) (sql.Node, error) {
	frame := v.stack[len(v.stack)-1]
	if cte, ok := n.(*sql.CTE); ok && cte.TableName != nil && len(v.stack) > 1 {
		v.stack[len(v.stack)-2].ctes[cte.TableName.Name] = struct{}{}
	}
	if _, ok := n.(*sql.WithClause); ok && len(v.stack) > 1 {
		for name := range frame.ctes {
			v.stack[len(v.stack)-2].ctes[name] = struct{}{}
		}
	}
	v.stack = v.stack[:len(v.stack)-1]
	_ = frame
	return n, nil
}

func (v *tenantVisitor) isCTE(name *sql.Ident) bool {
	if name == nil || len(v.stack) == 0 {
		return false
	}
	_, ok := v.stack[len(v.stack)-1].ctes[name.Name]
	return ok
}

func TestWalkTenantRewrite(t *testing.T) {
	original := `WITH source AS (SELECT id FROM source) UPDATE target AS t SET value = s.value FROM source AS s WHERE t.id = s.id AND s.bound = ?`
	stmt := parseStatement(t, original)
	root, err := sql.Walk(newTenantVisitor(), stmt)
	if err != nil {
		t.Fatal(err)
	}
	rewritten := root.String()
	reparsed := parseStatement(t, rewritten)
	expected := parseStatement(t, `WITH source AS (SELECT id FROM tenant_source) UPDATE tenant_target AS t SET value = s.value FROM source AS s WHERE t.id = s.id AND s.bound = ?`)
	normalizeStatement(t, reparsed)
	normalizeStatement(t, expected)

	update := reparsed.(*sql.UpdateStatement)
	expectedUpdate := expected.(*sql.UpdateStatement)
	if diff := deep.Equal(StripPos(update), StripPos(expectedUpdate)); diff != nil {
		t.Fatalf("rewritten AST mismatch:\n%s", strings.Join(diff, "\n"))
	}
	if update.Table.Name.Name != "tenant_target" || update.Table.Alias.Name != "t" {
		t.Fatalf("target rewrite = %q AS %q", update.Table.Name.Name, update.Table.Alias.Name)
	}
	cteTable := update.WithClause.CTEs[0]
	if cteTable.TableName.Name != "source" {
		t.Fatalf("CTE name rewritten to %q", cteTable.TableName.Name)
	}
	cteSource := cteTable.Select.Source.(*sql.QualifiedTableName)
	if cteSource.Name.Name != "tenant_source" {
		t.Fatalf("real table shadowed by CTE not rewritten: %q", cteSource.Name.Name)
	}
	outerSource := update.Source.(*sql.QualifiedTableName)
	if outerSource.Name.Name != "source" || outerSource.Alias.Name != "s" {
		t.Fatalf("CTE source rewritten to %q AS %q", outerSource.Name.Name, outerSource.Alias.Name)
	}
	assignment := update.Assignments[0].Expr.(*sql.QualifiedRef)
	var qualifiers []string
	collectQualifiedRefs(t, update, &qualifiers)
	if assignment.Table.Name != "s" {
		t.Fatalf("SET column qualifier changed: %q", assignment.Table.Name)
	}
	wantQualifiers := []string{"s", "t", "s", "s"}
	if diff := deep.Equal(qualifiers, wantQualifiers); diff != nil {
		t.Fatalf("column qualifiers mismatch: %s", strings.Join(diff, "\n"))
	}
	if strings.Count(rewritten, "?") != 1 {
		t.Fatalf("binding count = %d, want 1", strings.Count(rewritten, "?"))
	}
}

func collectQualifiedRefs(t *testing.T, n sql.Node, qualifiers *[]string) {
	t.Helper()
	_, err := sql.Walk(sql.VisitFunc(func(node sql.Node) (sql.Node, error) {
		if ref, ok := node.(*sql.QualifiedRef); ok {
			*qualifiers = append(*qualifiers, ref.Table.Name)
		}
		return node, nil
	}), n)
	if err != nil {
		t.Fatal(err)
	}
}

func normalizeStatement(t *testing.T, n sql.Node) {
	t.Helper()
	_, err := sql.Walk(sql.VisitFunc(func(node sql.Node) (sql.Node, error) {
		if ident, ok := node.(*sql.Ident); ok {
			ident.Quoted = false
		}
		return node, nil
	}), n)
	if err != nil {
		t.Fatal(err)
	}
}

type errorAtVisitor struct {
	path   string
	err    error
	events []string
	stack  []walkFrame
}

func (v *errorAtVisitor) Visit(n sql.Node) (sql.Visitor, sql.Node, error) {
	path := typeName(n)
	if len(v.stack) > 0 {
		path = v.stack[len(v.stack)-1].path + "." + childPath(v.stack[len(v.stack)-1].node, n)
	}
	v.stack = append(v.stack, walkFrame{path: path, node: n})
	v.events = append(v.events, "enter "+path)
	if ident, ok := n.(*sql.Ident); ok {
		ident.Name = "mark_" + ident.Name
	}
	if path == v.path {
		return nil, n, v.err
	}
	return v, n, nil
}

func (v *errorAtVisitor) VisitEnd(n sql.Node) (sql.Node, error) {
	path := v.stack[len(v.stack)-1].path
	v.events = append(v.events, "leave "+path)
	v.stack = v.stack[:len(v.stack)-1]
	return n, nil
}

func TestWalkSourceErrorsShortCircuit(t *testing.T) {
	tests := []struct {
		name  string
		query string
		path  string
	}{
		{
			name:  "root source",
			query: `UPDATE target SET value = s.value FROM source AS s WHERE target.id = s.id`,
			path:  "UpdateStatement.Source.QualifiedTableName",
		},
		{
			name:  "nested join",
			query: `UPDATE target AS t SET value = s.value FROM source AS s JOIN other AS o ON s.id = o.id WHERE t.id = s.id`,
			path:  "UpdateStatement.Source.JoinClause.Y.QualifiedTableName",
		},
		{
			name:  "subquery source",
			query: `UPDATE target AS t SET value = s.value FROM (SELECT id FROM source) AS s WHERE t.id = s.id`,
			path:  "UpdateStatement.Source.ParenSource.X.SelectStatement.Source.QualifiedTableName",
		},
		{
			name:  "table function argument",
			query: `UPDATE target AS t SET value = s.value FROM source_table(1) AS s WHERE t.id = s.id`,
			path:  "UpdateStatement.Source.QualifiedTableFunctionName.Args[0].NumberLit",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sentinel := errors.New("source subtree stopped")
			visitor := &errorAtVisitor{path: tt.path, err: sentinel}
			stmt := parseStatement(t, tt.query)
			_, err := sql.Walk(visitor, stmt)
			if err != sentinel {
				t.Fatalf("Walk error = %v, want %v", err, sentinel)
			}
			last := visitor.events[len(visitor.events)-1]
			if last != "enter "+tt.path {
				t.Fatalf("last event = %q, want enter %s", last, tt.path)
			}
			for _, event := range visitor.events[:len(visitor.events)-1] {
				if strings.HasPrefix(event, "enter UpdateStatement.WhereExpr") || strings.HasPrefix(event, "enter UpdateStatement.ReturningClause") {
					t.Fatalf("event after short-circuit: %s", event)
				}
			}
			if got := strings.Count(strings.Join(visitor.events, "\n"), tt.path); got != 1 {
				t.Fatalf("error node events = %d, want 1", got)
			}
			marked := markedIdentNames(stmt)
			var enteredIdents []string
			for _, event := range visitor.events {
				if strings.HasPrefix(event, "enter ") && (strings.HasSuffix(event, ".Ident") || event == "enter Ident") {
					name := identNameAtPath(stmt, strings.TrimPrefix(event, "enter "))
					enteredIdents = append(enteredIdents, name)
				}
			}
			sort.Strings(marked)
			sort.Strings(enteredIdents)
			if diff := deep.Equal(marked, enteredIdents); diff != nil {
				t.Fatalf("partial rewrite mismatch:\n%s", strings.Join(diff, "\n"))
			}
			for _, name := range marked {
				if strings.HasPrefix(name, "mark_mark_") {
					t.Fatalf("identifier %q was rewritten by an implicit second scan", name)
				}
			}
		})
	}
}

func markedIdentNames(n sql.Node) []string {
	var marked []string
	value := reflect.ValueOf(n).Elem()
	var walkValue func(reflect.Value)
	walkValue = func(v reflect.Value) {
		if !v.IsValid() {
			return
		}
		if v.Kind() == reflect.Ptr && v.Type() == reflect.TypeOf((*sql.Ident)(nil)) && !v.IsNil() {
			name := v.Elem().FieldByName("Name").String()
			if strings.HasPrefix(name, "mark_") {
				marked = append(marked, name)
			}
			return
		}
		switch v.Kind() {
		case reflect.Ptr, reflect.Interface:
			if !v.IsNil() && (v.Kind() != reflect.Interface || v.Elem().Kind() == reflect.Ptr) {
				if v.Kind() == reflect.Interface {
					walkValue(v.Elem())
				} else {
					walkValue(v.Elem())
				}
			}
		case reflect.Struct:
			for i := 0; i < v.NumField(); i++ {
				walkValue(v.Field(i))
			}
		case reflect.Slice:
			for i := 0; i < v.Len(); i++ {
				walkValue(v.Index(i))
			}
		}
	}
	walkValue(value)
	return marked
}

func identNameAtPath(n sql.Node, path string) string {
	current := reflect.ValueOf(n)
	parts := strings.Split(path, ".")
	for _, part := range parts[1:] {
		name := part
		index := -1
		if bracket := strings.Index(part, "["); bracket >= 0 {
			end := strings.Index(part, "]")
			_, _ = fmt.Sscanf(part[bracket+1:end], "%d", &index)
			name = part[:bracket]
		}
		if name != typeName(mustNodeValue(current)) {
			field := current.Elem().FieldByName(name)
			if index >= 0 {
				field = field.Index(index)
			}
			current = dereferenceNodeField(field)
		}
	}
	ident := current.Interface().(*sql.Ident)
	return ident.Name
}

func mustNodeValue(v reflect.Value) sql.Node {
	if n, ok := nodeValue(v); ok {
		return n
	}
	panic("path does not identify a node")
}

func dereferenceNodeField(v reflect.Value) reflect.Value {
	if v.Kind() == reflect.Interface {
		return v.Elem()
	}
	return v
}

func TestWalkMutations(t *testing.T) {
	if os.Getenv("SQL_WALK_MUTATION_CHILD") == "1" {
		t.Skip("mutation driver process")
	}
	tests := []struct {
		name   string
		mutate func(t *testing.T, code string) string
	}{
		{
			name: "remove source recursion",
			mutate: func(t *testing.T, code string) string {
				old := "func walkSource(v Visitor, x Source) (Source, error) {\n\tif rn, err := walk(v, x); err != nil {\n\t\treturn nil, err\n\t} else {\n\t\treturn rn.(Source), nil\n\t}\n}"
				replacement := "func walkSource(_ Visitor, x Source) (Source, error) {\n\treturn x, nil\n}"
				return mustReplace(t, code, old, replacement)
			},
		},
		{
			name: "place source after where",
			mutate: func(t *testing.T, code string) string {
				updateStart := strings.Index(code, "\tcase *UpdateStatement:")
				nextStart := strings.Index(code[updateStart:], "\n\tcase *UpsertClause:")
				if updateStart < 0 || nextStart < 0 {
					t.Fatal("cannot locate update statement switch case")
				}
				block := code[updateStart : updateStart+nextStart]
				sourceBlock := mustExtractBlock(t, block, "if nn.Source != nil {")
				whereBlock := mustExtractBlock(t, block, "if expr, err := walkExpr(v, nn.WhereExpr); err != nil {")
				sourceStart := strings.Index(block, sourceBlock)
				whereStart := strings.Index(block, whereBlock)
				if sourceStart < 0 || whereStart < 0 || sourceStart > whereStart {
					t.Fatal("invalid update source/where block positions")
				}
				sourceEnd := sourceStart + len(sourceBlock)
				whereEnd := whereStart + len(whereBlock)
				block = block[:sourceStart] + whereBlock + block[sourceEnd:whereStart] + sourceBlock + block[whereEnd:]
				return code[:updateStart] + block + code[updateStart+nextStart:]
			},
		},
		{
			name: "ignore source visitor error",
			mutate: func(t *testing.T, code string) string {
				old := "func walkSource(v Visitor, x Source) (Source, error) {\n\tif rn, err := walk(v, x); err != nil {\n\t\treturn nil, err\n\t} else {\n\t\treturn rn.(Source), nil\n\t}\n}"
				replacement := "func walkSource(v Visitor, x Source) (Source, error) {\n\trn, _ := walk(v, x)\n\treturn rn.(Source), nil\n}"
				return mustReplace(t, code, old, replacement)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			copyRepository(t, root)
			path := filepath.Join(root, "walk.go")
			code, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			mutated := tt.mutate(t, string(code))
			if err := os.WriteFile(path, []byte(mutated), 0o644); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(goTestBinary(t), "test", "./...", "-count=1", "-run", "TestWalk")
			cmd.Dir = root
			cmd.Env = append(os.Environ(),
				"SQL_WALK_MUTATION_CHILD=1",
				"GO111MODULE=on",
				"GOPROXY=off",
				"GOFLAGS=-mod=mod",
			)
			output, err := cmd.CombinedOutput()
			if err == nil {
				t.Fatalf("mutated code unexpectedly passed:\n%s", output)
			}
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) || exitErr.ExitCode() == 0 {
				t.Fatalf("mutation command error = %v, output:\n%s", err, output)
			}
		})
	}
}

func goTestBinary(t *testing.T) string {
	t.Helper()
	goBinary, err := exec.LookPath("go")
	if err != nil {
		t.Fatal(err)
	}
	return goBinary
}

func copyRepository(t *testing.T, root string) {
	t.Helper()
	files, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		if file.IsDir() && (file.Name() == ".git" || strings.HasPrefix(file.Name(), ".")) {
			continue
		}
		if file.IsDir() {
			if err := os.Mkdir(filepath.Join(root, file.Name()), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := copyTree(t, file.Name(), filepath.Join(root, file.Name())); err != nil {
				t.Fatal(err)
			}
			continue
		}
		copyFile(t, file.Name(), filepath.Join(root, file.Name()))
	}
}

func copyTree(t *testing.T, sourceRoot, targetRoot string) error {
	t.Helper()
	return filepath.Walk(sourceRoot, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(sourceRoot, path)
		if err != nil {
			return err
		}
		target := filepath.Join(targetRoot, relative)
		if info.IsDir() {
			return os.MkdirAll(target, info.Mode())
		}
		copyFile(t, path, target)
		return nil
	})
}

func copyFile(t *testing.T, source, target string) {
	t.Helper()
	code, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, code, 0o644); err != nil {
		t.Fatal(err)
	}
}

func mustReplace(t *testing.T, code, old, replacement string) string {
	t.Helper()
	count := strings.Count(code, old)
	if count != 1 {
		t.Fatalf("mutation target count=%d, want 1", count)
	}
	return strings.Replace(code, old, replacement, 1)
}

func mustExtractBlock(t *testing.T, code, prefix string) string {
	t.Helper()
	start := strings.Index(code, prefix)
	if start < 0 {
		t.Fatalf("cannot find block %q", prefix)
	}
	depth := 0
	for i := start; i < len(code); i++ {
		switch code[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return code[start : i+1]
			}
		}
	}
	t.Fatalf("unterminated block %q", prefix)
	return ""
}
