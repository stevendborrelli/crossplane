/*
Copyright 2025 The Crossplane Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package ordering

import (
	"fmt"
	"sort"
	"strings"
)

// A Decision says what the reconciler should do with one composed resource
// this reconcile, and why.
type Decision struct {
	// Blocked is true if the graph is holding this resource back.
	Blocked bool

	// BlockedBy names what it is waiting for. Empty unless Blocked.
	BlockedBy []string

	// Reason is a human-readable explanation, suitable for a condition
	// message. Empty unless Blocked.
	Reason string

	// Deadlocked is true if this resource cannot become unblocked by waiting.
	// The graph and the pipeline's desired state contradict each other, so
	// Crossplane must surface the contradiction rather than stall on it.
	Deadlocked bool
}

// Decisions is the outcome of one ordering pass, keyed by composed resource
// name.
type Decisions struct {
	// Apply lists the resources the applier should patch, in any order.
	Apply []string

	// Delete lists the resources the garbage collector should delete.
	Delete []string

	// Blocked explains every resource the graph held back, whether it was
	// waiting to be applied or waiting to be deleted.
	Blocked map[string]Decision
}

// Decide returns what the reconciler should do this pass.
//
// The rule is the one in the design: the delete set is observed minus desired,
// and the graph only orders transitions that desired state has already
// authorised. A resource is created once everything it depends on is ready. A
// resource is deleted once everything that depends on it has left observed
// state. A resource that already exists is applied without consulting the
// graph - see blockedFromApply for why updates are not gated.
//
// # Cost
//
// This is deliberately a scan rather than an index, and the scan is over the
// whole edge set for every resource - including resources that have no edges
// at all. With n composed resources and m edges:
//
//   - creating costs O(n*m): blockedFromApply walks every edge once per
//     resource, looking for the ones that name it.
//   - deleting costs O(n*m*d), where d is the average number of dependents a
//     resource has. blockedFromDelete walks every edge to find the dependents
//     of one resource, then asks createBeforeDestroy about each dependent -
//     and that question is itself another walk of every edge.
//
// Both are quadratic in the size of a composition, because m grows with n.
// Measured on an M-series laptop (see decide_bench_test.go), one Decide call
// costs roughly 5ns per edge visited: 98us for a 100-resource chain, 1.4ms for
// 100 resources with ten edges each, 116ms for 1000 resources with ten edges
// each. Compare that against a function pipeline, which takes 1-100ms.
//
// So this is free at the scale compositions actually reach, becomes visible
// somewhere around 500 resources with a dense graph, and dominates the
// reconcile near 1000. Width costs far more than depth: ten edges per resource
// is 20x the cost of a chain at the same n. It is also per reconcile, so it
// multiplies with reconcile frequency - which is highest exactly when a large
// graph is converging.
//
// The fix, if a real composition ever gets there: build two maps once at the
// top of Decide - edges by e.Resource, and edges by the composed resource
// they depend on - and have blockedFromApply, blockedFromDelete, dependentsOf
// and createBeforeDestroy read those instead of ranging over g.edges. That
// turns every O(m) walk into an O(1) lookup and the whole pass into O(n+m).
// It is perhaps thirty lines. It is not worth the indirection until someone
// has a composition that needs it, and it should come with a benchmark
// showing the before and after rather than being taken on faith.
func (g *Graph) Decide(s State) Decisions {
	d := Decisions{Blocked: map[string]Decision{}}

	names := make([]string, 0, len(s.Composed))
	for n := range s.Composed {
		names = append(names, n)
	}

	sort.Strings(names)

	for _, n := range names {
		cs := s.Composed[n]

		switch {
		case cs.Desired:
			if b := g.blockedFromApply(n, s); b != nil {
				d.Blocked[n] = *b
				continue
			}

			d.Apply = append(d.Apply, n)

		case cs.Observed:
			// Not desired but still present: a deletion candidate.
			if b := g.blockedFromDelete(n, s); b != nil {
				d.Blocked[n] = *b
				continue
			}

			d.Delete = append(d.Delete, n)
		}
	}

	return d
}

// blockedFromApply reports whether n is waiting on something before it can be
// created.
//
// Ordering gates creation, not updates. A resource that already exists was
// reconciling before the graph had an opinion about it, and holding its
// updates back because something it depends on went un-ready takes away the
// change that might fix things - including a change that has nothing to do
// with the dependency. Deferring updates is arguably more correct, and core
// could do it, but the cost of being wrong is an operator who cannot move.
// Widening this later is additive; narrowing it once compositions rely on it
// is not.
func (g *Graph) blockedFromApply(n string, s State) *Decision {
	if s.Composed[n].Observed {
		return nil
	}

	waiting := []string{}

	// Requirements that matched nothing at all. A wait like any other, but one
	// that is answered by creating a resource rather than by waiting for one
	// to come up, so it is reported in its own words.
	empty := []string{}

	// Contradictions carry both what to blame and why: BlockedBy names the
	// thing, Reason explains it.
	contradicting := []string{}
	why := []string{}

	for _, e := range g.edges {
		if e.Resource != n {
			continue
		}

		switch t := e.DependsOn; {
		case t.RequiredResource != nil:
			switch requirementStatus(s, *t.RequiredResource) {
			case requirementUnmatched:
				// The edge names a resource nothing matched. Waiting cannot fix
				// that, and saying "not ready yet" would send someone looking at
				// a resource that is already ready. The usual cause is naming a
				// namespaced resource without its namespace.
				contradicting = append(contradicting, "requirement "+t.RequiredResource.RequirementName)
				why = append(why, describeUnmatched(*t.RequiredResource))

			case requirementEmpty:
				// Nothing matched at all. This resolves once the resource
				// exists, so it is a wait - but reporting it as "not ready"
				// sends someone to check the readiness of a resource that
				// isn't there.
				empty = append(empty, "requirement "+t.RequiredResource.RequirementName)

			case requirementUnfetched, requirementNotReady:
				waiting = append(waiting, "requirement "+t.RequiredResource.RequirementName)

			case requirementSatisfied:
			}

		case t.ComposedResource != "":
			dep := s.Composed[t.ComposedResource]

			// CreateBeforeDestroy exists precisely so a replacement need not
			// wait for its predecessor to go away.
			if !dep.Desired && e.CreateBeforeDestroy {
				continue
			}

			// A dependency the pipeline has dropped from desired state is on
			// its way out, while this resource still wants it. Waiting cannot
			// resolve that: the dependency will never become ready again, and
			// it cannot be deleted while this resource depends on it. Report
			// the contradiction instead of stalling silently.
			if !dep.Desired && dep.Observed {
				contradicting = append(contradicting, t.ComposedResource)
				why = append(why, fmt.Sprintf("%s is being deleted by the pipeline", t.ComposedResource))

				continue
			}

			if !dep.Desired || !dep.Ready {
				waiting = append(waiting, t.ComposedResource)
			}
		}
	}

	if len(contradicting) > 0 {
		sort.Strings(contradicting)
		sort.Strings(why)

		return &Decision{
			Blocked:    true,
			BlockedBy:  contradicting,
			Reason:     "cannot be satisfied: " + strings.Join(why, "; "),
			Deadlocked: true,
		}
	}

	if len(waiting)+len(empty) == 0 {
		return nil
	}

	sort.Strings(waiting)
	sort.Strings(empty)

	by := append(append([]string{}, waiting...), empty...)
	sort.Strings(by)

	reasons := []string{}
	if len(waiting) > 0 {
		reasons = append(reasons, fmt.Sprintf("waiting for %v to be ready", waiting))
	}

	if len(empty) > 0 {
		reasons = append(reasons, fmt.Sprintf("waiting for %v to match a resource", empty))
	}

	return &Decision{
		Blocked:   true,
		BlockedBy: by,
		Reason:    strings.Join(reasons, "; "),
	}
}

// blockedFromDelete reports whether n is waiting on its dependents to go away
// before it can be deleted.
func (g *Graph) blockedFromDelete(n string, s State) *Decision {
	// Dependents that must finish going away.
	waiting := []string{}

	// Create-before-destroy successors that must arrive first. They are waited
	// on for the opposite reason, so they are reported separately - telling
	// someone a resource is waiting for its own replacement to be deleted
	// sends them looking for the wrong thing.
	replacing := []string{}

	// Dependents the pipeline still wants. They are not on their way out, so
	// waiting for them to go away cannot end.
	kept := []string{}

	for _, dep := range g.dependentsOf(n) {
		ds := s.Composed[dep]

		// With CreateBeforeDestroy the replacement must exist and be ready
		// before its predecessor is torn down. Note this is checked before
		// the not-observed case below: a replacement that hasn't been created
		// yet is the reason to wait, not a reason to proceed.
		if g.createBeforeDestroy(dep, n) {
			if ds.Desired && ds.Observed && ds.Ready {
				continue
			}

			replacing = append(replacing, dep)

			continue
		}

		// A dependent that is still observed - including one that is
		// terminating - has not finished going away.
		if !ds.Observed {
			continue
		}

		waiting = append(waiting, dep)

		if ds.Desired {
			kept = append(kept, dep)
		}
	}

	if len(waiting)+len(replacing) == 0 {
		return nil
	}

	sort.Strings(waiting)
	sort.Strings(replacing)

	by := append(append([]string{}, waiting...), replacing...)
	sort.Strings(by)

	// The pipeline wants this resource gone and wants something that depends
	// on it to stay. Waiting cannot resolve that - the dependent is not
	// leaving - so report the contradiction rather than waiting forever on it.
	if len(kept) > 0 {
		sort.Strings(kept)

		return &Decision{
			Blocked:    true,
			BlockedBy:  by,
			Reason:     fmt.Sprintf("cannot be satisfied: the pipeline still wants %v, which depends on it", kept),
			Deadlocked: true,
		}
	}

	reasons := []string{}
	if len(waiting) > 0 {
		reasons = append(reasons, fmt.Sprintf("waiting for %v to be deleted", waiting))
	}

	if len(replacing) > 0 {
		reasons = append(reasons, fmt.Sprintf("waiting for %v to be created and ready", replacing))
	}

	return &Decision{
		Blocked:   true,
		BlockedBy: by,
		Reason:    strings.Join(reasons, "; "),
	}
}

// createBeforeDestroy reports whether the edge from resource to dependsOn sets
// the create-before-destroy flag.
func (g *Graph) createBeforeDestroy(resource, dependsOn string) bool {
	for _, e := range g.edges {
		if e.Resource == resource && e.DependsOn.ComposedResource == dependsOn {
			return e.CreateBeforeDestroy
		}
	}

	return false
}

// A requirement is how far along a required-resource dependency is. Several
// distinct situations look alike from the outside - all of them are "this edge
// isn't satisfied" - but they send whoever reads the XR to different places,
// so the gate tells them apart rather than reporting one wait for all of them.
type requirement int

const (
	// requirementUnfetched means Crossplane has not fetched this requirement
	// this reconcile. Transient: it resolves on a later pass, or on this one
	// once the function declares it.
	requirementUnfetched requirement = iota

	// requirementEmpty means the requirement was fetched and matched nothing.
	// Also transient - the resource may not exist yet - but for a different
	// reason, and someone reading "not ready" would go looking for a resource
	// that isn't there to look at.
	requirementEmpty

	// requirementUnmatched means the requirement matched resources, but not
	// the one this edge names. Waiting cannot fix that.
	requirementUnmatched

	// requirementNotReady means the resources this edge names were matched and
	// are not ready yet.
	requirementNotReady

	// requirementSatisfied means the edge is not holding anything back.
	requirementSatisfied
)

// requirementStatus reports how far along a required-resource dependency is.
func requirementStatus(s State, r RequiredRef) requirement {
	rs, ok := s.Required[r.RequirementName]
	if !ok {
		return requirementUnfetched
	}

	if len(rs.Ready) == 0 {
		return requirementEmpty
	}

	if r.Name != "" {
		ready, matched := rs.Ready[RequiredResourceKey(r.Namespace, r.Name)]

		switch {
		case !matched:
			return requirementUnmatched
		case !ready:
			return requirementNotReady
		default:
			return requirementSatisfied
		}
	}

	for _, ready := range rs.Ready {
		if !ready {
			return requirementNotReady
		}
	}

	return requirementSatisfied
}

// describeUnmatched explains an edge that names a resource nothing matched.
func describeUnmatched(r RequiredRef) string {
	if r.Namespace == "" {
		return fmt.Sprintf(
			"requirement %s matched no resource named %q; namespace is not set, and a namespaced resource needs one",
			r.RequirementName, r.Name)
	}

	return fmt.Sprintf("requirement %s matched no resource %s/%s",
		r.RequirementName, r.Namespace, r.Name)
}

// Retain implements the edge retention rule: an edge whose target is absent
// from desired state but still present in observed state is kept, even if the
// function that returned next dropped it. That is the pending-deletion case,
// and the only one where losing an edge causes damage.
func Retain(prev, next []Edge, s State) []Edge {
	have := map[string]bool{}
	for _, e := range next {
		have[key(e)] = true
	}

	out := append([]Edge{}, next...)

	for _, e := range prev {
		if have[key(e)] {
			continue
		}

		t := e.DependsOn.ComposedResource
		if t == "" {
			continue
		}

		if cs := s.Composed[t]; !cs.Desired && cs.Observed {
			out = append(out, e)
		}
	}

	return out
}

func key(e Edge) string {
	t := e.DependsOn.ComposedResource
	if e.DependsOn.RequiredResource != nil {
		t = "req/" + e.DependsOn.RequirementName()
	}

	return e.Resource + "->" + t
}

// RequirementName returns the requirement name a target refers to, or "".
func (t Target) RequirementName() string {
	if t.RequiredResource == nil {
		return ""
	}

	return t.RequiredResource.RequirementName + "/" + t.RequiredResource.Namespace + "/" + t.RequiredResource.Name
}
