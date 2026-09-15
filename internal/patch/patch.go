// Package patch builds JSON merge patches pinned to the read object's
// resourceVersion and uid (spec section 6): a stale read yields 409, a
// recreated object 422, a missing one 404. Callers re-read and recompute.
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

// IsStale reports whether err means the read is no longer current.
func IsStale(err error) bool {
	return apierrors.IsConflict(err) || apierrors.IsInvalid(err) || apierrors.IsNotFound(err)
}
