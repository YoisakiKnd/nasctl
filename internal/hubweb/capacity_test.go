package hubweb

import "testing"

func TestFilesystemCapacityReachesRootsAndOverview(t *testing.T) {
	r := newRig(t, nil)
	var roots rootsResponse
	r.getJSON(t, "/api/v1/files/roots", &roots)
	if len(roots.Roots) == 0 {
		t.Fatal("fixture has no root")
	}
	for _, root := range roots.Roots {
		if root.Available && (root.Capacity == nil || root.Capacity.TotalBytes <= 0 || root.Capacity.VolumeID == "") {
			t.Fatalf("API dropped real capacity: %+v", root)
		}
	}
	var overview overviewResponse
	r.getJSON(t, "/api/v1/overview", &overview)
	if len(overview.Files.Roots) != len(roots.Roots) || overview.Files.Roots[0].Capacity == nil {
		t.Fatal("overview dropped capacity")
	}
}
