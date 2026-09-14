// Package patch builds the conditional mutations every SHOCK lifecycle write
// uses (spec section 6, concurrency protocol): a JSON merge patch that carries
// only the intended field changes and pins both metadata.resourceVersion and
// metadata.uid of the object that was read. A stale resourceVersion yields
// 409 Conflict; a different UID (the object was recreated) yields 422 because
// uid is immutable; a missing object yields 404. Callers re-read and recompute
// on any of those, never replaying the old payload.
package patch

import (
	"encoding/json"
	"fmt"

	jsonpatch "github.com/evanphx/json-patch/v5"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Optimistic returns a merge patch from orig to mutated, pinned to orig's
// UID and resourceVersion. orig must be the object as read from the API.
func Optimistic(orig, mutated client.Object) (client.Patch, error) {
	if orig.GetResourceVersion() == "" || orig.GetUID() == "" {
		return nil, fmt.Errorf("patch: observed object %s/%s lacks uid or resourceVersion", orig.GetNamespace(), orig.GetName())
	}
	origJSON, err := json.Marshal(orig)
	if err != nil {
		return nil, err
	}
	mutJSON, err := json.Marshal(mutated)
	if err != nil {
		return nil, err
	}
	raw, err := jsonpatch.CreateMergePatch(origJSON, mutJSON)
	if err != nil {
		return nil, err
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	meta, _ := doc["metadata"].(map[string]any)
	if meta == nil {
		meta = map[string]any{}
	}
	meta["resourceVersion"] = orig.GetResourceVersion()
	meta["uid"] = string(orig.GetUID())
	doc["metadata"] = meta
	out, err := json.Marshal(doc)
	if err != nil {
		return nil, err
	}
	return client.RawPatch(types.MergePatchType, out), nil
}

// IsStale reports whether err means the read the patch was computed from is
// no longer current: conflict, immutable-field violation (UID changed) or
// the object having disappeared.
func IsStale(err error) bool {
	return apierrors.IsConflict(err) || apierrors.IsInvalid(err) || apierrors.IsNotFound(err)
}
