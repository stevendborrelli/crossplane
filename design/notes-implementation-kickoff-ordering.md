# Implementation Kickoff — Composed Resource Ordering

Companion to `design/design-doc-composed-resource-ordering.md`. These are
concrete starting points in *this* repo, verified against the current `main`
branch — treat line numbers as "start here," not gospel, since they drift.

This proposal's delete path depends on
[one-pager #7242](https://github.com/crossplane/crossplane/pull/7242),
function-controlled deletion. Until the pipeline runs during XR deletion, the
ordering below only applies to resources that fall out of desired state while
the XR is alive. Sequence the work accordingly.

## 1. Proto changes — `proto/fn/v1/run_function.proto`

* Add the `Dependencies`, `Dependency` and `RequiredResourceDependency`
  messages (full shape in the one-pager). `Dependency.depends_on` is a `oneof`
  over a composed resource name and a required resource reference, and
  `Dependencies` wraps the repeated field so unset can be told from empty.
* Add `Dependencies dependencies = 10;` to `RunFunctionRequest`, after
  `required_schemas = 9` (~L99).
* Add `Dependencies dependencies = 8;` to `RunFunctionResponse`, after
  `output = 7` (~L158).
* Pin the generators before regenerating: this repo's committed output comes
  from `protoc-gen-go` v1.36.11 and `protoc-gen-go-grpc` v1.6.2. Verify a
  no-op `buf generate` produces an empty diff first, or the real change is
  buried in version churn.
* Add a `CAPABILITY_DEPENDENCIES` value to the `Capability` enum, after
  `CAPABILITY_REQUIRED_SCHEMAS = 5` (~L203). Value `6` is only free if #7242
  hasn't landed; it renames the enum and claims `6`.
* Regenerate generated Go code, and mirror the same additions in
  `function-sdk-go`'s `proto/v1` package plus its request/response builder
  helpers, so functions don't have to hand-construct `Dependency` messages.
  The response builder must copy `dependencies` through by default.

## 2. Pipeline execution — `composite/composition_functions.go`

`FunctionComposer.Compose` (~L300) is the loop that runs each function in
sequence and threads `RunFunctionRequest`/`RunFunctionResponse` between steps.
Add here:

* **Carry-forward** — when a function's response leaves `dependencies` unset,
  default it to what that function's request held rather than treating it as
  empty. This is what stops an old, unaware function from silently erasing
  edges declared earlier in the pipeline. Note this is core's job, not the
  convention's: functions compiled before this field existed cannot know to
  echo it.
* **Edge retention** — separately from carry-forward, retain any edge whose
  `depends_on` endpoint is absent from desired state but still present in
  observed. A function that *does* have an opinion returns the whole list and
  will naturally drop edges for resources it is removing, which is exactly when
  the garbage collector needs them.
* **Validation** — after each response, check that every endpoint resolves
  against the union of `Desired.Resources` and `Observed.Resources` names, that
  every `requirement_name` corresponds to a requirement the pipeline actually
  declared, that no required resource appears as `resource`, that
  `create_resource_before_destroying_dependency` is unset on required-resource
  edges, and that the accumulated edge set stays acyclic. Report a violation as
  a warning event plus `Synced: False` on the XR naming the offending function
  — core cannot add a `Result` to a function's own response.

`AsState` (~L922) builds the `*fnv1.State` sent to functions. Two things to
check here: it constructs observed resources as
`&fnv1.Resource{Resource: ..., ConnectionDetails: ...}` and never sets `ready`,
so observed readiness is not available to functions today; and observed comes
from `ObserveComposedResources`, which reads `spec.resourceRefs`, so it does
include resources that have left desired state — which the union check above
relies on.

## 3. Where ordering actually gets enforced

* **Create/update.** The apply loop inside `FunctionComposer.Compose` (~L678,
  the `for name, cd := range desired` that calls `c.client.Patch`) needs the
  create-side gate: skip the patch until everything `cd` depends on is ready.
  Readiness for a composed resource is the pipeline's verdict,
  `ComposedResourceState.Ready` (set ~L622 from `dr.GetReady()`); for a
  required resource there is no verdict, so read the object's `Ready`
  condition, treating existence alone as sufficient for kinds that have none.
* **A skipped resource must still be reported.** That same loop appends
  `ComposedResource{ResourceName: name, Ready: ..., Synced: true}` to the slice
  `Compose` returns, and the reconciler uses it to compute the XR's `Synced`
  and `Ready` conditions. A resource held back by the graph must appear there
  with `Synced: false` and a reason, or the XR will report `Synced: True` while
  composition is deliberately incomplete.
* **Delete.** `DeletingComposedResourceGarbageCollector.GarbageCollectComposedResources`
  (~L958) computes `del` as `observed - desired`. Filter it: don't delete a
  resource while anything with a `depends_on` edge to it is still present in
  observed. A resource carrying a `deletionTimestamp` counts as present.
  Note the finalizer removal condition becomes "observed is empty", not
  "desired is empty" - under #7242 desired is empty from the first teardown
  pass, so keying on it would drop the finalizer while resources remain.
* **Don't reference a resource the graph is holding back from creation.**
  References are written before resources are applied, so a created resource
  can't be leaked - but a blocked resource was never applied, and referencing
  it points anything reading `spec.resourceRefs` at an object that doesn't
  exist. `crossplane resource trace` reports it as
  `Error: ... not found`, which reads as a failure rather than as waiting.
  Reference what exists plus what is about to be applied, and nothing else.
* **Deferred deletes must stay in `spec.resourceRefs`.** `UpdateResourceRefs`
  (~L1015) rebuilds the whole array from `desired` alone, and `Compose` calls
  it right after garbage collection (~L643). A resource whose deletion the
  graph defers is not in desired, so its reference is dropped, it isn't
  observed next reconcile, and it is never collected or reported. Refs must be
  the union of desired and the resources still present in observed. This bites
  hardest during XR teardown under #7242, where desired is empty and the first
  pass would otherwise wipe every reference.
* **Requeue.** `Reconciler.Reconcile` in `reconciler.go` (~L569) is the
  level-triggered outer loop. No new scheduling primitive is needed, but note
  *why*: with realtime compositions enabled the result is `RequeueAfter: 0`
  (~L912), so the pass that unblocks a resource comes from the watch
  established via `c.tracker.Track(xrKey, composed, required)` (~L783). That
  call already tracks required resources as well as composed ones, matched by
  name or label, so required-resource edges get their unblock event for free.

## 4. `function-dependency-graph` — the adoption shim

Out of tree, but on the critical path: without something populating
`dependencies`, none of the above is reachable by an existing Composition. Fork
`crossplane-contrib/function-sequencer` and keep its input schema so migrating
is a one-line `functionRef` change.

* **Translate rules to edges.** A `sequence` of *n* names becomes an edge from
  each element to every predecessor, not just the adjacent one — that is
  sequencer's existing semantics (`for _, before := range sequence[:i]` in its
  `fn.go`), and adjacent-pairs-only would weaken it.
* **Expand regex at translation time**, against the names present in desired
  and observed state, so the wire format stays exact-name-only.
* **Detect the capability.** With `CAPABILITY_DEPENDENCIES` advertised, emit
  edges and pass desired state through untouched. Without it, fall back to
  sequencer's current behavior exactly: hold resources back by omission, emit
  `Usage` objects when `enableDeletionSequencing` is set, honor
  `resetCompositeReadiness`. One binary, both cores.
* **`resetCompositeReadiness` becomes a no-op in graph mode.** Core reports
  blocked resources itself, which is the point. Keep accepting the field so
  existing Compositions don't break; log that it is ignored.
* **Leave `deleteOnly`, `createOnly` and `condition` rules on the legacy
  path.** These three decouple or disable one direction of a sequence, which a
  symmetric edge can't express, and the one-pager deliberately doesn't grow the
  protocol to cover them. Translate only plain rules into edges; evaluate the
  rest exactly as `function-sequencer` does today. Both kinds can coexist in
  one step.
* **Keep `cacheTTL`.** It exists because large Compositions re-run the pipeline
  often; nothing here changes that.

## 5. Prototype

`internal/xfn/ordering` models the decision logic - validation, the apply and
delete gates, edge retention - with no dependency on the protocol or the
reconciler, so the semantics can be exercised before either changes. Run it
with `go test ./internal/xfn/ordering/`. Its `simulate` helper replays
Decide across passes and asserts the wave-per-reconcile behavior the one-pager
claims.

Two traps it already caught, both worth carrying into the real implementation:

* **Check create-before-destroy before the not-observed short circuit.** In the
  delete gate it is tempting to skip any dependent that isn't observed, on the
  grounds that something which doesn't exist can't block anything. That is
  right for a normal edge and wrong for a create-before-destroy edge, where a
  replacement that has not been created yet is precisely the reason to hold the
  predecessor back. Getting this backwards deletes the old resource before the
  new one exists, which is the failure the flag exists to prevent.
* **A contradictory graph deadlocks silently.** See the one-pager's
  "Consumption" section. Detect the shape and report it; don't let the XR sit
  in a stall that no amount of waiting will clear.

**Open: cycle detection is a second implementation.** The prototype hand-rolls
a colour-marking DFS (~40 lines across `acyclic`, `nodes`, `dependenciesOf`
and `dependentsOf`) rather than reusing `internal/dag`. The reasons were that
`dag.Node` requires `GetConstraints`, `GetParentConstraints` and
`AddParentConstraints` — semantic-version concerns with no meaning for a
composed resource — and that `MapDag`'s useful output is a topological sort,
which the gate doesn't need: eligibility is "does this resource's dependencies
look ready right now," recomputed each reconcile against live state, not a
position in a total order. Neither reason is decisive. Two cycle detectors in
one repo is a real maintenance cost, and `internal/dag` already has a fuzz
corpus this doesn't. If this proposal lands, the tidy fix is to extract the
DFS from `MapDag` over a plain `map[string][]string` adjacency and have both
callers share it, leaving the `Node` interface as a layer on top. Not worth
refactoring working package-manager code before then.

## 6. Observability

Blocking is silent by default: a dependency that never becomes ready stalls its
dependents with nothing explaining why. Ship this with the gating, not after.
Minimum is a machine-readable reason on the `ComposedResource` entries, that
reason surfaced in the XR's `Synced` condition message, and an event when a
resource has been blocked for an extended period.

## Suggested order of work

1. Proto, generated code, and `function-sdk-go` builders — small, mechanical,
   unblocks everything else.
2. Carry-forward, edge retention, and validation in `FunctionComposer.Compose`
   — correctness-critical, no visible behavior change yet.
3. Wire the graph into the create/apply path, including the `Synced: false`
   reporting and the blocked-reason plumbing.
4. Fix `UpdateResourceRefs` to retain deferred deletes, then wire the graph
   into `GarbageCollectComposedResources`.
5. Required-resource edges: validation, readiness reading, and confirming the
   existing tracker actually delivers the unblock event.
6. Keep `internal/xfn/ordering` in step with the real implementation, or
   fold it in and delete it once the reconciler owns the logic.
7. Fork `function-sequencer` into `function-dependency-graph`, with the
   capability-gated fallback. This is what makes the feature reachable from a
   Composition without every function in the pipeline being updated, so it
   should not trail the core work by much.
8. End-to-end tests. Three things this needs that aren't obvious:

   * **A function that emits `dependencies`.** No published function can - the
     field is new - so one has to be built and published. There's a working one
     in `test/e2e/functions/ordering` with a `build.sh` that produces a
     multi-arch xpkg. It's compiled against this repo's `proto/fn/v1`, so a
     proto change means republishing or tests fail confusingly.
   * **`provider-nop` for deterministic timing.** Composing ConfigMaps makes
     every ordering wave complete inside one second, so a test can only assert
     eventual convergence - which passes even with ordering disabled. Composing
     `NopResource`s with `conditionAfter` spreads the waves over seconds, and
     creation timestamps then show the ordering directly. Note the gaps aren't
     uniform: the first wave lands at the configured delay, later ones wait on
     provider-nop's poll interval. Assert order, never elapsed time.
   * **Assert on resources, not events.** Kubernetes aggregates and drops rapid
     duplicate events, and a teardown wave completes well inside a second. In
     testing, create-side gating reached the API as events while delete-side
     gating never did. `ResourcesDeletedAfterListedAreGone` in
     `test/e2e/funcs` already composes the deletion-order assertion.

   Coverage to write: a function that declares an edge — verify creation waits,
   verify deletion order reverses, verify
   `create_resource_before_destroying_dependency` flips it, verify a blocked
   resource reports why. Deletion-order coverage on XR teardown depends on
   #7242.
