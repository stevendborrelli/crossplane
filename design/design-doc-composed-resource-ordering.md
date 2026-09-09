# Composed Resource Ordering

* Owner: Stefano Borrelli (@stevendborrelli)
* Reviewers: Crossplane Maintainers
* Status: Draft
* Issue: [#5092](https://github.com/crossplane/crossplane/issues/5092)

This document proposes implementing resource dependency in Crossplane
Composition reconciler so that Composed Resources can be created and deleted
based on a dependency graph.

## Background

Crossplane's Composition engine does not support the ordered creation and
deletion of resources by design. It follows Kubernetes patterns where
resources are created asynchronously and controllers operate to implement
the requested desired state. If an AWS network is created in a Composition,
the VPC, Subnets, and Routes are created in parallel. When
the Composite Resource is deleted all the composed resources are given
deletionTimestamps in parallel.

Unfortunately, the infrastructure Crossplane manages outside the cluster
is not as forgiving as Kubernetes resources. There are many instances
when infrastructure platforms need to track the dependencies between
resources under management:

* Blocking deletion of a Kubernetes Cluster until all the workloads running on
  it have been deleted.
* Delaying the creation of Users and Schemas into a Database until the Database
  has been provisioned and is healthy.
* Predicting the scope of proposed infrastructure changes in regulated
  environments to reduce risk.

These requirements have led to the development of a number of solutions within
Crossplane and the broader community:

* Using a
  [`fromFieldPath.policy: Required`](https://docs.crossplane.io/latest/guides/function-patch-and-transform/#fromfieldpath-policy)
  in patch-and-transform to delay rendering of a Managed Resource until a field
  is present.
* [Usages](https://github.com/crossplane/crossplane/blob/main/design/one-pager-generic-usage-type.md)
  for deletion ordering.
* Provider [Managed Resource
  References](https://github.com/crossplane/crossplane/blob/main/design/one-pager-cross-resource-referencing.md)
  cause a Managed Resource to return an error until the Referred resource
  exists.
* Within function logic using the Observed state of other Objects in a
  conditional statement.
* Functions like
  [function-sequencer](https://github.com/crossplane-contrib/function-sequencer)
  create ordering by controlling the rendering of desired state.
* Via function logic as in
  [function-pythonic's](https://github.com/crossplane-contrib/function-pythonic#composed-resource-dependencies)
  automatic dependency creation.
* Using tools like
  [Kyverno](https://github.com/crossplane/crossplane/discussions/4072) to block
  deletion of a resource.

While these solutions solve real problems, they place the responsibility on the
Crossplane Community, who must piece together various solutions and introduce
complexity into their environments.

With the success of
[Composition
Functions](https://github.com/crossplane/crossplane/blob/main/design/design-doc-composition-functions.md),
I believe Crossplane can implement a resource graph in the Composition engine,
significantly reducing the use of Usages and other workarounds.

### Ordering in Composition Functions

Let's review how ordering works today in practice. Composition Functions run as
an ordered pipeline of unary gRPC calls. Each
function receives, via `RunFunctionRequest`, the full desired and observed
state accumulated by the functions before it, and is required to pass forward
anything it doesn't have an opinion on. Composed resources are identified by
name, as keys into `State.Resources`.

`RunFunctionRequest.observed.resources` carries the full composed resource
objects, including their status conditions, so a function can control resource
creation on observed state: don't add the Subnet to desired state until the VPC
appears in observed state as ready. Crossplane never creates the Subnet until
the function says so. This ordering requires custom logic as in
[configuration-aws-eks](https://github.com/upbound/configuration-aws-eks/blob/5b6c3d384fc9c2c66223f4433dd6bc1ba6478d70/functions/eks/main.k#L222).

While the XR is alive — a function that stops returning a resource in desired
state causes Crossplane to garbage collect it, so resources can be torn down a
wave at a time. Once an XR is deleted, the function is no longer invoked.

As mentioned previously, `function-sequencer` is another method to let a
Composition author declare ordering as data in a pipeline step's input:

```yaml
rules:
  - sequence: [vpc, subnet, security-group]
```

It reads accumulated desired state, and holds a resource back — by removing it
from desired — until every predecessor is ready. Because it operates on
composed resource names in state rather than on anything a particular function
produced, it works with any function that came before it in the pipeline,
templating functions included, and it sequences resources that different
pipeline steps contributed. For deletion it composes `Usage` resources, which
block deletion at admission until the dependent is gone.

A similar technique is followed by function-pythonic, skipping rendering on
creation and Creating Usages to control deletion.

### Ordering in Managed Resources

Providers also have a method of defining dependencies: Managed Resource
References where key fields in one Managed Resource can be obtained from
another Resource in the same provider family. In the case of a VPC->Subnet
dependency both resources will be created in parallel, but until the VPC is
ready the Subnet will emit an error against the Kubernetes API server that the
VPC is not ready yet. Managed Resource References support three lookup methods:

* `Id`: the cloud provider's id for the resource, usually the Crossplane
  [external-name](https://docs.crossplane.io/latest/concepts/managed-resources/#naming-external-resources)
  annotation.
* `IdRef`: A Reference to a Kubernetes Managed Resource object by Name and
  Namespace. The Kubernetes Kind and API Group is hardcoded in the provider,
  and the object is fetched from the API Server by the provider. The provider
  then extracts the ID and updates the spec of the requesting object.
* `IdSelectors`: Instead of using a Kubernetes Name/Namespace the Provider can
  match multiple objects on the cluster. The Provider proceeds to fetch each
  matching resource and extract the required field.

An example use of Managed Resource References can be seen in the KCL
implementation of
[configuration-aws-network](https://github.com/upbound/configuration-aws-network):

```kcl
 ec2v1beta1.RouteTableAssociation{
        metadata = _metadata("rta-" + _formatSubnet(s)) | {
            labels = {
                "networks.aws.platform.upbound.io/network-id" = oxr.spec.parameters.id
            }
        }
        spec = _defaults | {
            forProvider = {
                region = oxr.spec.parameters.region
                routeTableIdSelector = {
                    matchControllerRef = True
                }
                subnetIdSelector = {
                    matchControllerRef = True
                    matchLabels = {
                        if s.type == "private":
                            access = "private"
                        else:
                            access = "public"
                        zone = s.availabilityZone
                    }
                }
            }
        }
```

Note that the `matchControllerRef` configures the behavior of the reference: if
set to `true` it looks within the composition. Otherwise it looks for the
resource outside of the composition. `matchControllerRef` predates Crossplane
Functions and Required Resources and overlaps them in functionality:

* Functions have access to the Observed state of the Composition during every
  execution and can extract any fields directly.
* Required Resources can import state from Resources outside the composition.
  Crossplane is responsible for the fetching of the resources instead of the
  Provider.

Managed Resource Refs have several downsides: There is no support for deletion
ordering and the knowledge of the relationship is kept in the Provider. They must
be implemented in the Provider and there is no strong typecasting in the CRDs
for the relationship. Function authors must delegate responsibility to the
Provider, and during provisioning an API error is issued for each resource
until the Requirements are fulfilled.

### Problems with Current Approaches

These examples demonstrate that while resource ordering is possible, it is
ad-hoc and inconsistent. This leads to less-than-optimal results in an
infrastructure platform:

* **"Not desired" and "desired, but later" are equivalent.** A function
  delays a resource by omitting it from desired state, which is
  indistinguishable from deciding the resource shouldn't exist. Crossplane
  therefore reports an XR as `Synced` and possibly `Ready` while composition is
  deliberately incomplete.
  
  `function-sequencer` ships a
  `resetCompositeReadiness` flag specifically to paper over this, described
  in its own documentation as
  preventing the XR from "entering the `Ready` state prematurely when there are
  pending resources that the composite reconciler is unaware of." The
  workaround is opt-in, so the default is wrong.

* **Ordering stops at creation.** Removing a live resource from desired state
  would delete it, so a function that delays by omission can only ever order
  creates. `function-sequencer` skips any resource already present in observed
  state, deliberately and correctly. Ordering updates is out of reach for the
  technique, not merely unimplemented — though this proposal chooses not to
  order them either, for reasons of its own. `function-pythonic` supports creating
  dependencies using a combination of selective rendering and creation of
  `Usage` objects.

* **Deletion ordering costs a custom resource per protected Resource.**
  Expressing teardown order through `Usage` objects means one `Usage` per
  dependency pair — per *match*, where patterns are involved — each with its
  own reconcile, finalizer and admission round trip. It also requires deleting
  the XR with `--cascade=foreground`, which is the one propagation mode that
  defeats function-controlled deletion (see "Interaction with
  Function-Controlled Deletion" below).

  In a complex project like
  [modelplane](https://github.com/modelplaneai/modelplane) a single deployment
  can contain over a dozen `Usage` objects on the cluster.

* **It is difficult to test changes to Managed Resource References.** Function
  authors must use Observed Resources during testing to simulate ordering.

These workarounds are what function authors must do today when there is no way
to declare dependencies in a Composition.

To address this gap an explicit, functions-declared dependency graph over
composed resources will be developed so that ordering can be declared as
ordering, and Crossplane's own applier and garbage collector can enforce and
report on it.

**Note**
On XR deletion none of the above applies anyway, because the reconciler removes
its finalizer as soon as it sees a `deletionTimestamp` and defers to Kubernetes
garbage collection, which cascades in no particular order. That is the subject
of [one-pager #7242](https://github.com/crossplane/crossplane/pull/7242), which
this proposal builds on rather than duplicates.

## Goals

* Give ordering a first-class representation in the function protocol, so that
  "this resource is waiting" is distinguishable on the wire from "this resource
  is not wanted."
* Make Crossplane's own reporting correct by construction: an XR whose
  composition is incomplete because a resource is blocked should say so,
  without a Composition author opting into a workaround.
* Let Crossplane's reconciler sequence real create and delete calls: don't
  create a resource until what it depends on is ready, and don't delete one
  until everything that depends on it is gone. Both are enforced against the
  API calls themselves rather than through desired state, so the XR can report
  honestly on what it is waiting for. See "Resolved Design Questions" for why
  updates are not gated.
* Let a composed resource depend on a resource the XR doesn't compose but does
  require, so ordering isn't limited to what a single XR owns.
* Express deletion ordering without a custom resource per edge, and without
  requiring `--cascade=foreground`.
* Default to the common case — destroy in the reverse of create order — with an
  explicit, opt-in escape hatch for the less common case where a replacement
  should be created before its predecessor is destroyed.
* Keep the mechanism consistent with how the rest of the function pipeline
  already works: full state per step, not a delta/merge protocol.
* Stay backward compatible with functions built before this proposal existed,
  and reach Compositions whose functions are never updated at all — see
  "Adoption" below.

## Non-Goals

* Ordering the *deletion* of resources that belong to different Composite
  Resources. Enforcement is scoped to the resources a single XR composes, since
  those are the only ones Crossplane may delete on its behalf. A composed
  resource may still be gated on the readiness of a resource the XR requires,
  including another XR — see "Depending on a required resource."
* Replacing observed-state gating. Gating remains the mechanism for anything
  conditional on more than existence and readiness — waiting for a Job to
  report success, for example. A function that gates today keeps working
  unchanged. This proposal adds a declarative option, not a replacement.
* Replacing patches as the way to move data between resources. This proposal
  only adds a way to express sequencing; it doesn't change how values flow.
* Changing how a provider determines a managed resource's readiness. For
  composed resources this proposal consumes the readiness verdict the pipeline
  already produces rather than defining a second one — see "Consumption"
  below.
* Making the function pipeline run during XR deletion. That is #7242's
  subject, and this proposal's delete-ordering depends on it. What this
  proposal adds is the order to tear down in — including, per "Graph-driven
  teardown without function cooperation," an order that holds even when the
  pipeline's functions know nothing about deletion.

## Proposal

Implement a dependency graph in the core rendering
engine and populate this graph with a new `dependencies` field returned from the
function pipeline.

Resources can depend on other resources in the composition, or on resources
outside the composition, using Crossplane's required resources functionality.

### Updating `RunFunctionRequest` and `RunFunctionResponse`

Add a top-level `dependencies` field to `RunFunctionRequest` and
`RunFunctionResponse`, alongside `desired`, `observed`, and `context`:

```protobuf
message Dependency {
  // Name of the composed resource that has the dependency.
  // A key into State.resources.
  string resource = 1;

  // Name of the composed resource it depends on.
  // A key into State.resources.
  string depends_on = 2;

  // If true, `resource` may be created without waiting for `depends_on`
  // to be deleted. `resource` must still exist and be Ready before
  // `depends_on` is deleted. See "Escape hatch" below.
  bool create_resource_before_destroying_dependency = 3;
}

// Wrapped in a message, not a bare repeated field, so that unset can be told
// apart from empty. See "Presence" below.
message Dependencies {
  repeated Dependency items = 1;
}

message RunFunctionRequest {
  // ...existing fields: meta = 1, observed = 2, desired = 3, input = 4,
  // context = 5, extra_resources = 6 [deprecated], credentials = 7,
  // required_resources = 8, required_schemas = 9...
  Dependencies dependencies = 10; // next unused field number
}

message RunFunctionResponse {
  // ...existing fields: meta = 1, desired = 2, results = 3, context = 4,
  // requirements = 5, conditions = 6, output = 7...
  Dependencies dependencies = 8; // next unused field number
}
```

A `Dependency` carries no copy of either resource — `resource` and `depends_on`
are the same string keys already used in `State.Resources`. That keeps the
marginal cost of this feature low: a Composite with a few dozen
composed resources rarely has more than a handful of ordering constraints per
resource, so `dependencies` stays small even though `desired` and `observed`
already carry full resource payloads on every call. An edge list this size is
inexpensive to send in full at every step, so there is no need to invent a delta
protocol to make it affordable.

Because edges are declared over names rather than over the resources
themselves, any function in the pipeline can constrain any pair of composed
resources, including resources contributed by an earlier step it doesn't
otherwise touch. The target of an edge is widened below to also allow a
resource the XR requires rather than composes.

### Depending on a Required Resource

An edge whose endpoints are both composed resources can only sequence things
the XR itself owns. It cannot express "don't create resource X until resource
A, which this XR doesn't compose, is ready" — a shared VPC, a cluster-scoped
policy, a database another team's XR manages. That constraint is common enough
that leaving it out would make the graph feel arbitrary.

Crossplane already has the plumbing. A function declares
`requirements.resources`, core fetches the matching objects and returns them in
`RunFunctionRequest.required_resources`, and the  core already
tracks those resources for watches, matched by name or label, alongside the
XR's composed resources. The event that unblocks a required-resource
dependency therefore already reaches the reconciler; nothing new is needed to
make a blocked resource wake up. As a replacement for Managed Resource
References lookups and resource fetching are removed from the Provider Pod and
into the Crossplane engine.

The target of an edge becomes a `oneof`:

```protobuf
message Dependency {
  // Name of the composed resource that has the dependency.
  // A key into State.resources.
  string resource = 1;

  // What `resource` depends on.
  oneof depends_on {
    // Name of another composed resource. A key into State.resources.
    string composed_resource = 2;

    // A resource the pipeline required, rather than composed.
    RequiredResourceDependency required_resource = 4;
  }

  // If true, `resource` may be created without waiting for `depends_on`
  // to be deleted. Only valid when depends_on is a composed resource.
  bool create_resource_before_destroying_dependency = 3;
}

message RequiredResourceDependency {
  // The requirement name. A key into RunFunctionRequest.required_resources,
  // and into a RunFunctionResponse's requirements.resources.
  string requirement_name = 1;

  // Optional name of a single resource within the set the requirement
  // matched. If unset, every matched resource must be ready.
  optional string name = 2;

  // Namespace of name for a namespaced resource. Leave unset for a
  // cluster-scoped resource.
  optional string namespace = 3;
}
```

Four semantics follow from the fact that the XR doesn't own a required
resource:

* **It only supports Creation ordering** Crossplane must never delete a
  resource it doesn't compose, so these edges constrain only the create and
  update direction. The delete direction is undefined for them, and
  `create_resource_before_destroying_dependency` is invalid on an edge whose
  target is a required resource — core rejects it in validation.
* **The reverse direction is invalid too.** A required resource may not appear
  as `resource`. Crossplane doesn't apply it, so it has nothing to sequence.
* **A requirement that matched nothing is unsatisfied**, not vacuously
  satisfied. Core returns an empty `Resources` message when a requirement
  matched no objects, which is exactly the "the thing I'm waiting for isn't
  there yet" case. Blocking is the safe reading. When a requirement matches
  several objects and `name` is unset, all of them must be ready. A
  namespaced resource identified by `name` must also set `namespace`; the pair
  is unambiguous even when the requirement matched identical names in different
  namespaces. Cluster-scoped resources leave `namespace` unset.
* **Readiness is read from the object.** A required resource has no pipeline
  verdict — it isn't in desired state and no function reports readiness for it.
  Core reads the object instead: ready means a `Ready: True` status condition,
  or, for object kinds that have no `Ready` condition at all, existence. The
  ready-ness of required resources is currently an open issue in this proposal.
  When a function declares an edge and its requirement in the same response,
  core evaluates the edge against the final requirement state fetched during
  that function run, rather than waiting for a later reconciliation.

Validation gains one check: a `requirement_name` must correspond to a
requirement the pipeline actually declared. An edge naming a requirement no
function requested is the same class of error as an edge naming a composed
resource that exists in neither desired nor observed state. Required resources
are always leaves in the graph, so they cannot introduce cycles.

**Requirement names in edges are pipeline-wide.** They are not, elsewhere in
core: each step's request carries only its own requirements, fetched from its
own bootstrap declarations and its own tracked ones, so two functions can use
the same name for different resources without interfering. An edge, though,
may name a requirement any function in the pipeline declared — that is what
makes it useful for a function to order a resource against something a
different function fetched.

That choice has to say what a name two steps both used means. Core unions the
resources rather than letting the last step to run overwrite the first. Where
both steps matched the same resource, which is what a shared name usually
indicates, the union is that resource and nothing changes. Where they matched
different ones, an edge naming it waits for all of them — the same
conservative reading the protocol already takes for a requirement that matches
several resources with no `name` set. Overwriting would instead let one
function's resource silently answer for a name another function used, and a
resource could be applied while the requirement its author meant was not
ready.

This facilitates the non-goal about ordering across Composite Resources. If the
resource an XR requires happens to be another XR, this expresses "don't create
my resource until that XR is ready" read-only, one-directional, and enforced
only on the create side. Sequencing the deletion of resources across XR
boundaries remains out of scope, and remains what `Usage` is for.

### Accumulation follows the existing full-state-per-step convention

Every function receives the full `dependencies` list accumulated so far and
returns the full list it wants going forward, the same way `desired` already
works. A function with no ordering opinion copies `request.dependencies` into
`response.dependencies` unchanged. SDKs should make this the default behavior
of their request and response builders, so unaware function code doesn't have
to do anything special to avoid dropping edges it doesn't understand.

**Edges must outlive the resources they order.** Carry-forward, described
below, protects a function that leaves `dependencies` unset. It does not
protect against a function that has an opinion: such a function returns the
whole list, and will naturally emit edges only for the resources it still
desires. Dropping a resource from desired state and dropping its edges in the
same response is the obvious thing to write, and it destroys the ordering at
exactly the moment the garbage collector needs it. Functions should keep
emitting edges for resources they are removing, and SDK helpers should make
that the default. Because it is easy to get wrong, core additionally retains
any edge whose `depends_on` endpoint is absent from desired state but still
present in observed state — the pending-deletion case, and the only one where a
dropped edge causes damage. Edges are otherwise free to be retracted.

### Backward compatibility: the runtime carries the field forward

`desired`'s copy-forward convention works today only because every function
that has ever existed was written after `desired` already existed in the
protocol — there is no bootstrapping problem. `dependencies` doesn't have that
luxury: a function compiled against an SDK version that predates this proposal
has no way to know it is expected to echo the field, regardless of what the
spec says going forward. If Crossplane's core treated a function leaving
`dependencies` unset in its response as "clear it," a single old, unaware
function anywhere in the pipeline would silently erase every ordering
constraint declared before it.

The fix belongs in the runtime, not the convention: when building the request
for pipeline step N+1, Crossplane defaults `dependencies` to what step N
received if step N's response left the field unset. "Unset" means "unchanged,"
never "empty." A function only needs to touch `dependencies` when it actually
has an opinion about ordering.

By convention many Function SDK's implement a `to()` function that sets
the response at the start of a pipeline. These should be updated to include the
dependencies from previous functions in the pipeline as an additional
improvement.

**Follow Current Function Presence Guidelines.** That distinction only works if
the wire format can carry it, which is why `dependencies` is a `Dependencies`
message wrapping a repeated field rather than a repeated field directly. Proto3
has no presence for repeated fields: an empty list and an unset field both
serialize to zero bytes and arrive as nil. A bare repeated field would
therefore make "no opinion" and "no constraints" indistinguishable — and,
worse, *distinguishable in process*, since an empty Go slice isn't nil until it
has been through a round trip. A function author testing with `crank render`
would see behavior that differs in a cluster. `State` wraps desired and
observed resources for exactly this reason; `Dependencies` follows the
precedent.

### Validation: core checks reference validity and acyclicity, once

After each function's response, before building the next request, Crossplane
checks:

1. Every `resource` and `depends_on` name is resolved against the *union* of
   `Desired.Resources` and `Observed.Resources` — not `Desired` alone. A
   composed resource dropped from `Desired`, because a function decided it
   should be deleted, but still present in `Observed` is exactly the case where
   a dependency edge matters most: it is what tells the reconciler not to
   delete it yet.
2. An edge naming a composed resource absent from *both* is **pruned, not
   rejected**. A function that returns a fixed set of rules — which
   is what a declarative shim over resource names does — keeps declaring edges
   for resources it has finished deleting, on every reconcile after teardown
   completes. Rejecting those would fail composition permanently. A typo and a
   completed deletion are indistinguishable from core's position, so the safe
   reading is to drop the edge. An edge naming an undeclared *requirement* is
   still an error, but "declared" must mean declared by any function in this
   pipeline run, not merely present in the request. A function returns
   `requirements.resources` and the edges over them in the same response, and
   Crossplane fetches required resources only after seeing that response, so on
   a first reconcile the request carries none of them. Validating against the
   request alone rejects every required-resource edge on its first pass, and
   since that happens before Crossplane records what the pipeline requires, it
   never recovers.
3. The accumulated edge set remains acyclic.

A violation is reported the way Crossplane reports other invalid function
output: the pipeline run fails, and Crossplane records a warning event and a
`Synced: False` condition on the XR, naming the function whose response
introduced the violation. Note that Crossplane cannot add a `Result` to a
function's own response — only functions populate that field. Doing both checks
once, centrally, means every SDK doesn't need to separately implement, and
potentially get wrong, the same validation.

### Consumption: the reconciler's applier and garbage collector

The graph's primary consumer isn't the next function in the pipeline — it is
Crossplane's own applier and garbage collector, already running as part of the
level-triggered reconcile loop.

* **Create and update.** The applier issues a patch for a composed resource
  only once every resource it `depends_on` is ready.
* **Delete.** The garbage collector issues a delete only once every resource
  that `depends_on` it has left observed state.

A resource blocked this reconcile is picked up on a later pass, so no new
scheduling primitive is needed. With realtime compositions enabled the
reconciler does not requeue on a fixed interval. The later pass comes from the
watch Crossplane already establishes on each composed resource through its
dependency tracker: a dependency becoming ready is itself the event that
unblocks its dependents.

**Which readiness.** "Ready" here means the pipeline's own verdict — the value
Crossplane already derives from `Resource.ready` in the final desired state and
uses to compute the XR's `Ready` condition. That keeps a single definition of
composed resource readiness rather than introducing a second: a function that
already influences readiness, `function-auto-ready` being the common case,
influences gating the same way. Reading the `Ready` status condition directly
off the observed object was the alternative; it was rejected because it would
silently disagree with the readiness the same pipeline reports on the XR. A
resource absent from desired state has no verdict, and so never satisfies a
dependency. Required resources are the documented exception: they have no
verdict either, so core reads their status directly — see "Depending on a
required resource."

**Blocked resources must still be reported.** `Compose` returns a
`[]ComposedResource` that the reconciler uses to compute the XR's `Synced` and
`Ready` conditions, and today that slice is built from the resources the
applier actually patched. A resource held back by the graph has to appear in it
as not synced, with a reason. Otherwise the XR reports `Synced: True` while
composition is deliberately incomplete.

**A graph can contradict desired state, and the contradiction must be
surfaced.** If the pipeline drops a resource from desired state while another
resource that depends on it stays desired, neither can move: the dependency
will never be ready again, so its dependent is blocked from applying, and the
dependent still exists, so the dependency is blocked from deleting. Waiting
cannot resolve this. Crossplane must recognize the shape — a dependency absent
from desired but present in observed, with a dependent still desired — and
report it as an error on the XR rather than stalling silently. The prototype
in `internal/xfn/ordering` marks these decisions `Deadlocked` for exactly
this reason; it was not obvious until the simulation deadlocked.

**A resource held back from creation must not be referenced.** Crossplane
records a reference to each composed resource before applying it, so that a
resource it created is never lost. A resource the graph is holding back was
never applied, so that reasoning doesn't apply to it - and referencing it
points every reader of `spec.resourceRefs` at an object that doesn't exist.
`crossplane resource trace` renders those as errors, so an XR waiting patiently
on its dependencies looks broken. The rule is to reference what exists, plus
what is about to be applied.

**Deferred deletes must stay in `spec.resourceRefs`.** Crossplane rebuilds the
XR's `spec.resourceRefs` from desired state, and observes composed resources
next reconcile by reading it. A resource whose deletion the graph defers is by
definition not in desired state, so it would drop out of the references array
and become invisible on the next pass: never observed, never collected, never
reported. References must be the union of desired state and the resources still
present in observed state.

### Observability: why a resource is blocked

The failure mode of this feature is silence. A dependency that never becomes
ready blocks its dependents indefinitely, and with nothing surfacing that, the
XR simply appears to be doing nothing. Whatever explains a blocked resource has
to ship in the same release as the gating, not as a fast-follow.

The minimum is that the `ComposedResource` entries above carry a stable reason
identifying the resource that is blocking, and that the reason reaches the XR's
`Synced` condition message. This includes deferred deletions: they are absent
from desired state but still block convergence, so core reports them as
unsynced until their dependents are gone.

Events are a supplement, not the mechanism. Testing a ten-resource graph in a
kind cluster, the create-side gating surfaced as events, but the delete-side
gating never reached the API at all - Kubernetes aggregates and drops rapid
duplicate events, and with realtime compositions a teardown wave completes in
well under a second. The reasons were only visible in Crossplane's logs. State
that persists on the XR is what makes a blocked resource debuggable; an event
is best-effort by design. Richer
presentation — rendering the graph in `kubectl describe`, or a per-resource
status condition — is discussed under "Future Considerations."

### Escape hatch: create-before-destroy for the asymmetric case

The default for every edge is symmetric: forward order for create, strict
reverse for delete. That covers the common cases — a VPC before a subnet, a
subnet before an instance. Some replacements need the opposite on the way out,
usually to avoid downtime or to work around a uniqueness or quota constraint,
where the new resource should exist before the old one is torn down.

Rather than introduce a second edge type,
`create_resource_before_destroying_dependency` is a per-edge boolean, mirroring
Terraform's `create_before_destroy` lifecycle flag. When set, the reconciler
may create `resource` without waiting for `depends_on` to be deleted first, but
must still wait for `resource` to exist and be ready before deleting
`depends_on`. This keeps the graph a single, simple structure: the flag changes
only which side of a transition may proceed early, not the topology.

## Adoption: reaching Compositions whose functions never change

A protocol field is only useful once something populates it. If declaring
edges required every function author to adopt a new SDK version and emit
`dependencies`, the feature would arrive slowly and unevenly — and #7242's
all-or-nothing capability check means one un-updated function in a pipeline is
enough to disable ordered teardown for the whole XR.

The proposal therefore includes a translation shim:
**`function-dependency-graph`**, a fork of **`function-sequencer`**, that
keeps its input schema and its user-facing behavior but emits
`dependencies` edges instead of mutating desired state.

### How it works

The fork reads the same `rules` block Composition authors already write, and
translates it into edges over composed resource names:

```yaml
  - step: order-resources
    functionRef:
      name: function-dependency-graph
    input:
      apiVersion: sequencer.fn.crossplane.io/v1beta1
      kind: Input
      rules:
        - sequence: [vpc, subnet, security-group]
```

becomes the edge set `subnet -> vpc`, `security-group -> vpc`,
`security-group -> subnet`. Note that this is every predecessor pair, not just
adjacent ones, matching `function-sequencer`'s existing semantics: a resource
at position *i* waits on all of `sequence[:i]`, not only on `sequence[i-1]`.

Three properties make this the right shape for adoption:

* **No other function has to change.** The shim is a pipeline step operating on
  composed resource names in accumulated state. Whatever produced those
  resources — a templating function, a general-purpose function, a function
  compiled years before this proposal — is unaffected and unaware. A
  Composition adopts the graph by changing one `functionRef`.
* **Pattern matching stays out of the protocol.** `function-sequencer`'s regex
  rules expand against the names present in desired and observed state at
  translation time, so the wire format can stay exact-name-only while authors
  keep writing `first-subresource-.*`. Expansion is a user-experience concern;
  edges are the interchange format. This is a better split than teaching core
  to match patterns.
* **It degrades to today's behavior.** If Crossplane doesn't advertise the
  dependencies capability, the fork does exactly what `function-sequencer` does
  now — hold resources back by omission, compose `Usage` objects for teardown,
  and set `resetCompositeReadiness` if configured. One function works against
  old and new cores, and the same Composition keeps working through an upgrade.

In graph mode `resetCompositeReadiness` becomes unnecessary: core knows a
resource is blocked and reports it, which is the whole point of moving the
signal into the protocol.

Two of `function-sequencer`'s inputs have no equivalent in a symmetric edge.
Its per-rule `deleteOnly` and `createOnly` modifiers decouple the create and
delete directions of a sequence, and its per-rule CEL `condition` disables
creation sequencing while deliberately keeping teardown order. Rather than
grow the protocol a lifecycle knob per case, the fork keeps those rules on the
legacy path: a rule that sets any of the three is evaluated the way
`function-sequencer` evaluates it today, and only plain rules become edges. A
Composition can mix both in one step, and nothing an author already wrote
stops working. If demand for asymmetric edges turns out to be real, it is an
additive field later — but the evidence for it is one contrib function's
options, not a constraint the graph cannot otherwise meet.

### Graph-driven teardown without function cooperation

There is a stronger consequence worth putting in front of #7242's author,
because it changes that proposal's fallback rule.

One-pager #7242 requires every function in the pipeline to advertise
`FUNCTION_CAPABILITY_DELETION` before Crossplane will run the pipeline on XR
deletion. The check is all-or-nothing because an unaware function keeps
returning its full desired state during teardown, so nothing would ever be
garbage collected and the XR would hang.

A declared graph removes that dependency on cooperation. When an XR is being
deleted, every composed resource is going away; the only open question is the
order, and the graph answers it without consulting desired state at all.
Crossplane could therefore fall back through three levels rather than two:

1. **Every function advertises deletion support.** #7242's model. Desired state
   drives teardown, so functions can also do work during it — run a backup Job,
   drain a queue — and the graph orders whatever desired state releases.
2. **Not every function advertises it, but the pipeline declared a graph.**
   Crossplane tears down every composed resource, ignoring desired state, in
   reverse graph order. Unaware functions may return whatever they like; it is
   discarded. Ordering is preserved, but nothing may do work during teardown.
3. **Neither.** Today's behavior: remove the finalizer, let Kubernetes cascade.

Level 2 is what makes ordered teardown available to the large set of
Compositions that will never have a fully updated pipeline. It is also a change
to #7242's design rather than a consequence of it, so it needs agreement there
before either proposal depends on it.

## Interaction with Function-Controlled Deletion

[One-pager #7242](https://github.com/crossplane/crossplane/pull/7242),
"Function-Controlled Deletion of Composed Resources," proposes running the
function pipeline when an XR is deleted. Today the XR reconciler removes its
finalizer as soon as it sees a `deletionTimestamp` and defers to Kubernetes
garbage collection, which cascades to composed resources via owner references
in no particular order. #7242 makes capability advertisement bidirectional:
when every function in the pipeline advertises `FUNCTION_CAPABILITY_DELETION`,
Crossplane runs the pipeline during deletion and honors the desired state
functions return.

That makes #7242 a prerequisite for the delete half of this proposal rather
than a competing design. The ordering described in "Consumption" above can only
take effect on XR teardown — the case that motivates this proposal — if the
pipeline runs during teardown at all. The two mechanisms compose as follows.

### Desired state decides what; the graph decides when

Functions have one lever over composed resource lifecycle, and it is the same
lever in both proposals: desired state. Observed state is built by core from
the XR's `spec.resourceRefs` and live reads against the API server. A function
cannot remove a resource from observed state, and should not express deletion
by removing edges from the graph. A resource leaves observed only once it is
actually gone from the cluster.

Deletion therefore needs no special rule. The rule core already applies while
the XR is alive extends unchanged:

```text
delete set   = observed − desired    (the function decides who goes)
delete order = graph, leaves first   (core decides when each one goes)
```

`GarbageCollectComposedResources` already computes `observed − desired`. This
proposal filters that set: only issue a delete for a resource with no remaining
dependents in observed. Under #7242 the same filter applies during teardown,
where desired shrinks toward empty and the delete set grows to include
everything the XR composed.

This is also why the validation rule above resolves names against the union of
`Desired.Resources` and `Observed.Resources` rather than `Desired` alone.
During teardown the graph must keep referring to resources that exist only in
observed. For the same reason, a function must keep returning its full edge
list while the XR is deleting. Dropping edges for resources it has removed from
desired discards the ordering at precisely the moment it is needed.

### Worked example: ordered teardown

A function composes a VPC and a Subnet, declares `subnet depends_on vpc`, and
advertises `FUNCTION_CAPABILITY_DELETION`. The user deletes the XR. On every
pass the function returns empty desired state and its unchanged edge list.

| Pass | `observed − desired` | Graph filter | Core does |
| --- | --- | --- | --- |
| 1 | `{vpc, subnet}` | `subnet` is a leaf, `vpc` is not | delete `subnet` |
| 2 | `{vpc}` | `vpc` is now a leaf | delete `vpc` |
| 3 | `{}` | — | remove the finalizer |

Under #7242 alone the same teardown requires the function to return `{vpc}` on
the first pass, observe on the second pass that the Subnet has left observed
state, and only then return empty desired. The sequencing state machine lives
in the function. With a declared graph it lives in core, and the function's
deletion logic reduces to returning empty desired state and keeping its edges.

### What the graph does not cover

An ordering edge expresses sequence, not completion. #7242 also aims to let a
function do work during teardown — its example is running a backup Job and
waiting for it to finish before the database it backs up is deleted. The edge
direction for that is natural: `backupJob depends_on database` yields create
order database then Job, and the reverse on the way out. But if the function
drops both from desired at once, core will correctly delete the Job before the
database without waiting for the Job to succeed.

Gating on completion still requires function logic: keep both the database and
the Job in desired state until the Job reports success in observed state, then
drop both and let the graph order the teardown. The division of labor is:

* This proposal handles **ordering** — the mechanical part, and the part a
  templating function can participate in by emitting edges.
* #7242 handles **work during teardown** — the part that genuinely needs a
  per-function state machine.

Neither subsumes the other.

That is what the shim in "Adoption" addresses, and it is why the three-level
fallback described there matters more than it might first appear. A pipeline
containing one un-updated function cannot reach level 1 no matter what the
other steps do, and no amount of SDK adoption fixes a function whose author has
moved on. A declared graph lets such a pipeline still tear down in order,
because the graph alone determines the sequence.

### Why the all-or-nothing check needs the shim

The capability check in #7242 is all-or-nothing: one function that does not
advertise `FUNCTION_CAPABILITY_DELETION` returns the whole XR to today's
unordered teardown. Its own text expects templating functions such as
`function-go-templating` and `function-kcl` not to advertise it.

Those functions can already get ordering by pairing with `function-sequencer`,
so what they lack is not ordering but the ability to take part in #7242's
teardown, which needs every step to advertise the capability. A pipeline with
one un-updated function cannot reach level 1 however good the other steps are,
and no amount of SDK adoption fixes a function whose author has moved on. That
is what the shim addresses, and why the three-level fallback above matters.

### Precedence when both mechanisms are active

A function may gate on observed state and declare edges over the same
resources. Both mechanisms then influence whether a resource is applied or
deleted, and they can disagree — a stale or incorrect edge could hold back a
delete the function is waiting on, and the function will not drop the next
resource from desired until that delete completes.

The rule is that desired state wins. The graph orders transitions that desired
state has already authorized; it never authorizes one on its own, and it never
holds back a resource whose lifecycle the function is itself sequencing.
Concretely, core applies the ordering filter only to the `observed − desired`
set, and treats the graph as advisory for any resource the function has kept in
desired state.

### Ordering only constrains Crossplane's own calls

The graph tells Crossplane's applier and garbage collector what to do. It is
not an admission control mechanism, so it does not stop anyone else. A
`kubectl delete` aimed straight at a composed resource succeeds even while
something depends on it; Crossplane notices afterwards and recreates it, but
the dependent has already spent time pointing at something that wasn't there.

`Usage` does prevent this, because a validating webhook rejects the call
whoever made it. That is a real advantage, and it is the strongest argument for
keeping Usages for anything that has to hold against out-of-band deletion
rather than merely sequence Crossplane's own work.

### Foreground deletion bypasses ordering

One-pager #7242 documents a limitation this proposal inherits verbatim. An
explicit `kubectl delete --cascade=foreground` on the XR causes Kubernetes to
preemptively delete dependents carrying `blockOwnerDeletion: true` as soon as
the owner has a `deletionTimestamp`, racing the pipeline. Ordering guarantees
are lost in that mode, degrading to current behavior. Background propagation,
the default, is safe: dependents are not touched until the owner is actually
removed, which the XR's finalizer prevents.

The reconciler changes the delete path needs - keeping deferred deletes in
`spec.resourceRefs`, and what counts as still present in observed - are in
`design/notes-implementation-kickoff-ordering.md`.

### Capability numbering

One-pager #7242 renames the `Capability` enum to `CrossplaneCapability`,
re-prefixes its values, and claims value `6` for
`CROSSPLANE_CAPABILITY_DELETION`. If it landsd first, the addition proposed
here becomes `CROSSPLANE_CAPABILITY_DEPENDENCIES = 7`. The table below
assumes the enum as it exists on `main` today, and should be read as
contingent on that ordering.
The wire format is unchanged either way — only numeric values matter for
serialization, and the two additions do not otherwise overlap.

## API Impact and Capabilities

This proposal changes a wire protocol that every installed Composition Function
and every running Crossplane core already depends on, so its compatibility
impact needs to stand on its own rather than be inferred from the rest of this
document. Two claims, both checked against this repo's current `main`:

**No new API version is required.** Adding a field with a previously-unused
number is wire-compatible by construction — that is what protobuf's evolution
model is for. `proto/fn/v1/run_function.proto` has already grown this way
several times without ever moving to a `v2` package: `required_resources`
shipped in Crossplane v1.15, `credentials` in v1.16, `conditions` in v1.17, and
`required_schemas` alongside the `capabilities` mechanism itself in v2.2 — all
additive fields on the same v1 messages. This proposal is one more instance of
that pattern, not an exception to it.

**Runtime behavior compatibility is handled by the existing capability
mechanism, extended by one value.** `RequestMeta.capabilities` already exists
so a function can tell "this core doesn't support X" apart from "this core
predates capability advertising entirely" — which is what the
`CAPABILITY_CAPABILITIES` value itself is for. Without a capability flag here,
a function talking to an older core would have its `dependencies` silently
accepted on the wire, because
protobuf servers don't reject fields they don't recognize, but never enforced,
with nothing telling the function that happened. A new capability value closes
that gap the same way it is already closed for every other optional feature in
this protocol.

At a glance, the concrete additions to `proto/fn/v1/run_function.proto` and
where they land:

* **`Dependency`** and **`RequiredResourceDependency`** — new top-level
  messages. No existing message changes shape.
* **`RunFunctionRequest.dependencies`**, field `10`. Additive; unset on old
  clients, and carried forward by the runtime rather than by the function.
* **`RunFunctionResponse.dependencies`**, field `8`. Additive; unset means
  "no opinion," not "empty."
* **`CAPABILITY_DEPENDENCIES`**, `Capability` enum value `6`. Additive; lets a
  function detect an older core that will accept the field without enforcing
  it.

The capability's name and value are contingent on #7242, which renames the
enum and claims value `6` — see "Capability numbering" above.

Functions should treat the capability's absence as "this core will not enforce
ordering," not as an error, the same graceful-degradation posture the protocol
already expects for every other optional capability.

## Resolved Design Questions

**Ordering gates creation, not updates.** `function-sequencer` orders creates
only, because its mechanism cannot do otherwise: removing a live resource from
desired state would delete it. Core has no such constraint and can defer an
update as easily as a create, and deferring updates is arguably more correct —
a subnet's CIDR change probably should wait for its VPC to settle.

It is not what this proposal does, because the failure modes are not
symmetrical. An update that lands earlier than ideal is usually absorbed: the
resource reconciles again when its dependency settles. An update that is
deferred indefinitely takes away the operator's ability to act — including,
in the worst case, the change that would fix the un-ready dependency. A
dependency that flaps for an unrelated reason, an expired credential say,
would freeze every resource downstream of it. That is a bad trade for a
correctness gain that a second reconcile would have delivered anyway.

The asymmetry that settles it is which direction is additive. Gating creates
only and widening later is a new field on `Dependency` and no change to
anything already written. Gating everything and narrowing later breaks
compositions that came to rely on it. When the two options are not clearly
separable on merit, take the one that leaves the other reachable.

Concretely: a resource present in observed state is applied regardless of what
its edges say. A resource that has never been created waits. Deletion ordering
is unaffected — it does not have this problem, because a resource that is
being deleted is not one anybody is waiting to update.

One consequence is worth naming. A contradiction — the pipeline dropping a
resource while keeping something that depends on it — used to be reported by
blocking the dependent. The dependent now keeps reconciling, so the
contradiction is reported where the deadlock actually is: on the resource that
can never be deleted, because the thing depending on it is not going away.

## Future Considerations

* **Per-edge update gating.** If deferring updates turns out to matter for a
  real composition, a function should be able to ask for it edge by edge
  rather than for the whole graph. That is an additive field on `Dependency`
  with a create-only default, so nothing written against this proposal
  changes meaning. Not worth designing before someone needs it.

* **Rendering the graph for humans.** Beyond the blocked-resource reporting
  that ships with this proposal, exposing the graph itself — in `kubectl
  describe` on the XR, or as a per-resource status condition — would help
  platform teams reason about a large Composition. Additive once the mechanism
  exists.
* **Readiness in observed state.** Crossplane builds the observed `State` it
  sends to functions without populating each `Resource.ready`, so a function
  that wants to know whether an observed composed resource is ready has to read
  its status conditions itself. Populating that field would make observed-state
  gating easier to write and would align what functions see with what core uses
  for gating. Independent of this proposal, but adjacent.
* **Typed edges.** This proposal treats every edge as a plain ordering
  constraint. If a real need emerges to distinguish a hard blocking dependency
  from an advisory ordering preference, that is an additive field on
  `Dependency`, not a breaking change — not worth speculatively designing now.
* **Telling a typo from a teardown.** Pruned edges are reported on the XR as
  an event counting them and naming their endpoints, so a rule that never
  matches anything is not entirely silent. It is still indiscriminate: an
  edge whose resources have finished being deleted is pruned for the same
  reason and reported the same way, so an ordinary teardown produces the
  event every reconcile while it runs. Prune cannot distinguish the two cases
  - that is why it prunes rather than errors - so telling them apart needs
  something else, such as reporting a rule that has never matched anything
  across the life of the XR rather than one that does not match right now.

## Prior Art

* **`function-sequencer`** (crossplane-contrib) is the closest prior art and
  the direct inspiration for this proposal's shape. It lets a Composition
  author declare ordering as data in a pipeline step's input, expands regex
  patterns over resource names, gates creation on the *pipeline's* readiness
  verdict rather than on the observed object's conditions — the same choice
  this proposal makes — and skips any resource already present in observed
  state so that delaying a create can never delete a live resource. For
  teardown it composes `Usage` objects and requires `--cascade=foreground`. Its
  `resetCompositeReadiness` flag and `cacheTTL` knob are each evidence of a
  constraint this proposal tries to remove. Its per-rule `deleteOnly` and
  `createOnly` modifiers, which decouple the two directions of a sequence, are
  deliberately *not* carried into the protocol — see "Adoption" for how the
  proposed fork handles rules that set them.
* **`function-deletion-protection`** (crossplane-contrib) composes a `Usage`
  for any resource annotated
  `protection.fn.crossplane.io/block-deletion: "true"`, and can run either as a
  pipeline step or as an Operation. It blocks deletion rather than ordering it,
  so it isn't an alternative to this proposal, but it is prior art for two
  patterns: functions using `Usage` as their enforcement mechanism, and
  declaring lifecycle intent by annotating a resource rather than by a separate
  rules block. The annotation style is worth weighing against an edge list for
  ergonomics.
* **Crossplane's `Usage` and `ClusterUsage`** (`apis/protection/`) express a
  deletion-ordering constraint directly: while a `Usage` records that B uses A,
  an admission webhook refuses to delete A, and `replayDeletion` re-issues a
  deferred delete once the block clears. Because enforcement is at admission
  rather than in the composition pipeline, Usages apply even when the XR itself
  is deleted — which is why both functions above build on them.
* **Terraform** builds a single dependency graph from resource references and,
  since resources are normally destroyed in the reverse of the order they were
  created, walks it in reverse for `destroy` by default. `create_before_destroy`
  is a per-resource lifecycle flag that flips that default for a specific
  resource, rather than a second graph or edge type. This proposal borrows both
  the "one DAG, reverse for teardown" default and the per-edge opt-out shape.
* **Pulumi** similarly derives an implicit dependency graph from resource
  references and topologically sorts it for both `up` and `destroy`, with
  `deleteBeforeReplace` as an escape hatch for the same class of exception.
* **Crossplane's own `RunFunctionRequest` and `RunFunctionResponse`** already
  establish the pattern this proposal follows: full state per unary call, "pass
  through what you don't have an opinion on," and a name-keyed map as the
  canonical way to refer to a composed resource. `dependencies` extends that
  pattern rather than introducing a new one.

## Alternatives Considered

* **Leaving ordering to `function-sequencer` as it exists today.** This is the
  serious alternative, and it already covers the common case: declarative
  rules, pattern matching, works with any upstream function, ships now, no core
  changes. Rejected not because it fails, but because two of the costs in
  "Background" are structural rather than incidental — an XR that misreports
  its own readiness unless an author opts into a flag, and teardown that costs
  a custom resource per edge and mandates `--cascade=foreground`. This proposal
  does not order updates either, so that difference is not one of the reasons. Each of those traces back to expressing
  ordering through a field that means something else. The proposed fork keeps
  everything that works about it; see "Adoption."
* **Composing `Usage` resources instead of declaring edges.** The mechanism
  `function-sequencer` and `function-deletion-protection` both use, and the
  serious alternative on the deletion side — it already ships, and its
  admission-time enforcement applies even when the XR itself is deleted, which
  nothing in the composition pipeline can currently match. The case for adding
  a second way to express deletion ordering is made in "Addressing Prior
  Objections" above rather than repeated here. Three costs that argue against
  it as the *general* mechanism: it costs a custom resource per edge — per
  *match*, with patterns — each with its own reconcile, finalizer and webhook
  round trip; it covers only the deletion direction, leaving creates
  unordered; and it binds to resources rather than to composed resource names,
  so an edge cannot be declared before both resources exist. Usages remain the
  right tool for deletion protection that isn't about ordering, and for the
  cross-XR cases this proposal doesn't address.
* **Leaving ordering to observed-state gating written by hand in each
  function.** Possible today, and what #7242 extends to XR deletion. Not
  rejected — it remains the right mechanism for anything conditional on more
  than existence and readiness, such as waiting for a Job to report success,
  and this proposal depends on it for the deletion path. Rejected as the *only*
  mechanism because it requires every function to implement a per-resource
  state machine, which is why `function-sequencer` exists at all.
* **Delta or patch messages between pipeline steps, instead of full state.**
  Rejected: it would introduce a second wire-level merge semantics alongside
  the one `desired` and `context` already use, forcing every SDK to hold two
  different mental models, and an edge-only graph over existing resource names
  is already cheap enough in full that the bandwidth a delta would save is
  marginal.
* **A second edge type for delete ordering, instead of a create-before-destroy
  flag.** Rejected: the topology practically never differs between create and
  delete order — only the direction of a specific transition does — and one
  graph is simpler for both SDK authors and the reconciler to reason about than
  two graphs that must be kept consistent with each other. `function-sequencer`
  offers a weaker version of the same idea in its per-rule `deleteOnly` and
  `createOnly` modifiers; this proposal keeps the edge symmetric rather than
  adding a third lifecycle knob alongside
  `create_resource_before_destroying_dependency`.
* **Expressing ordering entirely through patches.** Piping a value from one
  resource's status into another's spec forces an implicit order, and is how
  many Compositions sequence resources today. Rejected as the sole mechanism
  because it can only express an order that coincides with a data dependency.

## Addressing Prior Objections

Ordering has been proposed to this project before, more than once, and
rejected.

**The core objection, stated on
[#2439](https://github.com/crossplane/crossplane/issues/2439) (2021):** "We
have consciously avoided this level of dependency modelling in Crossplane —
i.e. having Crossplane create, delete, or update things in an ordered manner.
Doing so is not idiomatic for Kubernetes and would increase the complexity of
our logic significantly. Instead we typically prefer for each `ExternalClient`
to tolerate eventual consistency." This is correct as a default, and we're not
arguing against the default. A managed resource whose `ExternalClient`
gracefully handles a missing dependency — returning an ordinary "not found yet,
will retry" error — genuinely doesn't need Crossplane to order anything; the
reconciler's own retry loop already converges without help. The default's
precondition is that every `ExternalClient`, across an ecosystem of providers
of widely varying maturity, implements that graceful handling correctly. Where
it doesn't, or where getting it wrong isn't just cosmetic, is where this
proposal is aimed:

* Some provider APIs return a hard, terminal validation error rather than a
  retriable "not found" when a referenced dependency doesn't exist yet.
  Expecting every community provider to get this right is optimistic at
  ecosystem scale, and when it's wrong, the failure surfaces as a persistent,
  alerting error condition on the XR rather than a quiet "pending" state.

* Blind retries against a missing dependency cost real API quota. A composite
  that fans out to a dozen resources with one late dependency generates a dozen
  failed create attempts every reconcile until it resolves, against cloud APIs
  that meter and throttle. An explicit graph lets Crossplane simply not attempt
  the call until it's likely to succeed — better for API budget, and for an
  operator's alerting, since "waiting on a dependency" and "genuinely broken"
  currently look identical from outside.

* This graph only matters in the first place for pairs of resources with no
  data dependency between them — no status field to patch into a spec field.
  That's exactly the case where today's *implicit* ordering (a patch can't
  populate a field that doesn't exist yet) provides no protection at all,
  because there's no patch. Eventual consistency's usual safety net is the
  thing that's absent here.

**Ordered creation specifically has been proposed and closed without landing,
more than once**
([#2072](https://github.com/crossplane/crossplane/issues/2072), and raised
again in [#1782](https://github.com/crossplane/crossplane/issues/1782)).
There's no existing mechanism this half of the proposal would duplicate —
unlike deletion, below, there's no shipped equivalent to reconcile with. That
also means creation-ordering is the part of this proposal most likely to draw
the same objection again, and its case has to rest on the concrete failure
modes above, not on "ordering is generally nice to have."

**Ordered deletion was requested just as often**
([#1612](https://github.com/crossplane/crossplane/issues/1612),
[#2439](https://github.com/crossplane/crossplane/issues/2439),
[#3225](https://github.com/crossplane/crossplane/issues/3225)) **— but unlike
creation, it shipped**, as the `Usage`/`ClusterUsage` API
([#3393](https://github.com/crossplane/crossplane/issues/3393)). This changes
what we're actually proposing on the deletion side. `Usage` already lets a
Composition Function express "don't delete Y until X is gone" today — the
community
[`function-deletion-protection`](https://github.com/crossplane-contrib/function-deletion-protection)
function does exactly this, emitting
`Usage`/`ClusterUsage` objects as desired resources that a validating webhook
then enforces. `dependencies` isn't introducing deletion-ordering where none
exists; it's proposing a second way to express it, which needs its own
justification rather than a free pass:

* `Usage` enforces ordering by having a validating webhook reject a delete call
  Crossplane's garbage collector already issued, which means the GC keeps
  attempting deletes it will be told no to until the blocking `Usage` clears
  (`replayDeletion` softens this by immediately retrying the *next* attempt
  once it clears, but doesn't stop the GC from calling in the first place).
  `dependencies` lets the GC consult the graph before issuing the call at all —
  one fewer rejected-and-logged API round trip, and no dependency on a webhook
  being reachable at the exact moment of deletion.

* `Usage` is a separate object a function has to construct and correctly target
  via `matchControllerRef` or label selectors. On the same issue thread that
  led to `Usage` shipping, one of its own prototypers described the UID-and-
  label-tracking experience as "functional but not very elegant" and asked what
  a more native API would look like. Representing the edge as data alongside
  the resources it connects, keyed by the same resource names the pipeline
  already uses, is a more direct fit for how a function already thinks about
  what it's composing.

* None of this obsoletes `Usage`. It remains the right tool for deletion
  protection that isn't about ordering (a human declaring "don't delete this,"
  independent of any other resource), and for cross-composite cases this
  proposal explicitly doesn't cover (see Non-Goals). A reviewer could
  reasonably conclude the within-composite gap is too narrow to justify a new
  protocol field rather than, say, an SDK helper that constructs `Usage`
  objects on a function's behalf — that alternative is recorded below rather
  than dismissed.
