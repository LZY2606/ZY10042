# Changelog

## Unreleased

- Fixed `Walk` traversal of relation sources. Source trees now follow the same depth-first contract as other AST children. For statements with a `FROM` clause, source traversal occurs in the statement's structural position: `SELECT` visits columns, then `Source`, then `WHERE`; `UPDATE` visits the target, assignments, `Source`, and `WHERE`.
- Fixed nested source coverage. `JoinClause`, `ParenSource`, table-valued functions, subqueries, CTEs, and scalar `SelectExpr` nodes now expose all of their children to visitors exactly once on enter and once on leave.
- Made `CTE` a first-class AST `Node`. Visitor-based rewrite rules can distinguish CTE definition names and aliases from physical table references without a formatter or rewriter rescan.
- Preserved child error propagation from all source traversals. An error in any source subtree short-circuits immediately, returns the same error object, emits no later events, and does not implicitly walk the statement a second time.
- Root cause: source traversal was represented ad hoc rather than through one source recursion path, so the Walker child contract was incomplete. Simple `UPDATE ... FROM <table>` used the statement-specific source field, but `SELECT Source`, CTE children, and scalar `SelectExpr` children were omitted. The inconsistent coverage left joins, parenthesized sources, subqueries, table-valued functions, and CTE shadowing without a uniform guarantee of traversal, replacement, and child-error propagation.
- Test coverage gap: previous parser/string tests proved that `UPDATE ... FROM ...` parsed and formatted correctly, but did not record strict enter/leave events or mutate AST nodes through `Walk`. Those tests therefore could not detect that a visitor saw the target but failed to reach every node in the FROM subtree, or that a source rewrite was skipped or duplicated on later clauses.
