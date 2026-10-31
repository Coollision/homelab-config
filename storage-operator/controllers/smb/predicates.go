package smb

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

// SMBPVCPredicate passes PVCs carrying the smb-access label (any value), like the Python
// label_selector='smb-access' watch.
func SMBPVCPredicate() predicate.Predicate {
	return predicate.NewPredicateFuncs(func(o client.Object) bool {
		_, ok := o.GetLabels()["smb-access"]
		return ok
	})
}

// MigrationConfigMapPredicate passes only smb-operator-migration in the operator namespace.
func MigrationConfigMapPredicate(namespace string) predicate.Predicate {
	return predicate.NewPredicateFuncs(func(o client.Object) bool {
		return o.GetNamespace() == namespace && o.GetName() == MigrationConfigMap
	})
}

// VolumeTuple is the subset of Volume status the operator renders.
func VolumeTuple(o client.Object) [3]string {
	u, ok := o.(*unstructured.Unstructured)
	if !ok {
		return [3]string{}
	}
	get := func(f string) string {
		s, _, _ := unstructured.NestedString(u.Object, "status", f)
		return s
	}
	return [3]string{get("shareEndpoint"), get("shareState"), get("state")}
}

// VolumeStatusPredicate passes Volume updates in namespace whose (shareEndpoint, shareState,
// state) changed. Creates (incl. the initial list) and deletes are ignored, as in Python, where
// the first sighting of a volume only seeded last_seen and the periodic resync covered it.
func VolumeStatusPredicate(namespace string) predicate.Predicate {
	return predicate.Funcs{
		CreateFunc:  func(event.CreateEvent) bool { return false },
		DeleteFunc:  func(event.DeleteEvent) bool { return false },
		GenericFunc: func(event.GenericEvent) bool { return false },
		UpdateFunc: func(e event.UpdateEvent) bool {
			if e.ObjectNew == nil || e.ObjectNew.GetNamespace() != namespace {
				return false
			}
			return VolumeTuple(e.ObjectOld) != VolumeTuple(e.ObjectNew)
		},
	}
}
