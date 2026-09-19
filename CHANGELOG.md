# Changelog

## Unreleased

### Fixed
- `Walk` now traverses every non-nil `Source` subtree in depth-first SQL order.
  For SELECT and UPDATE, Source is visited after the projection/SET nodes and
  before WHERE. Join nodes visit the left source, operator, right source, and
  join constraint; parenthesized sources visit their nested source and alias;
  table-valued functions visit their name, arguments, and alias.
- `Walk` now traverses `WithClause` children and treats each CTE as an AST node.
  CTE visits follow the node order name, optional column list, then defining
  SELECT, allowing visitors to distinguish CTE names from real table names that
  CTEs shadow.
- Scalar/parenthesized subqueries represented by `SelectExpr` now visit their
  embedded SELECT statement, so correlated and uncorrelated subqueries are
  reached from expressions as well as FROM clauses.

### Compatibility
- Existing AST types, the `Walk`/`Visitor` interfaces, node replacement and
  pruning behavior, `Clone`, and `String` behavior are preserved. Newly reached
  CTE and Source nodes are additional children visited in their natural
  depth-first position; previously visited target, SET, and WHERE nodes retain
  their relative order and remain visited once.
- Visitor errors from any Source child still short-circuit immediately, return
  the same error object, and do not trigger a second traversal or partial
  re-scan.

### Testing
- Added strict enter/leave Walker coverage for single tables, aliases, nested
  joins, parenthesized sources, correlated and uncorrelated subqueries,
  table-valued functions, CTE/real-table name shadowing, no-FROM statements,
  and names shared by SET, FROM, and WHERE.
- Added a tenant identifier rewrite test that walks once, formats the result,
  reparses it, and verifies that CTE names, aliases, and column qualifiers stay
  unchanged while only semantically real table identifiers receive the tenant
  prefix.
- Added short-circuit tests at multiple Source depths and mutation tests that
  remove Source recursion, move Source after WHERE, and ignore child visitor
  errors.

### Root Cause
- The parser correctly constructed `Source` trees, including UPDATE FROM, but
  Walker coverage was node-specific rather than enforced through the shared
  Source contract. UPDATE and SELECT relied on separate branches, so Source
  recursion could be absent or moved without a cross-statement test catching
  it. `WithClause` was also a traversal leaf, and `SelectExpr` was not
  dispatched to its embedded SELECT.
- Existing UPDATE FROM tests asserted parsed AST structures and their
  `String()` output only. They verified that the parser retained the FROM tree,
  but never applied a Visitor, so the missing or incomplete Walker edges were
  not observed.
