package api

import (
	"testing"

	"dockback/internal/dockercli"
)

func sampleContainers() []*dockercli.Container {
	return []*dockercli.Container{
		{ID: "1", Name: "blog-postgres", Image: "postgres:16", State: "running", Stack: "blog", Service: "db"},
		{ID: "2", Name: "blog-redis", Image: "redis:7", State: "running", Stack: "blog", Service: "cache"},
		{ID: "3", Name: "web-nginx", Image: "nginx", State: "exited", Stack: "web", Service: "proxy"},
		{ID: "4", Name: "standalone", Image: "alpine", State: "paused"},
	}
}

func ids(cs []*dockercli.Container) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.ID
	}
	return out
}

func TestBuildContainerPageSearch(t *testing.T) {
	resp := buildContainerPage(sampleContainers(), "redis", "", 1, 0)
	if resp["total"].(int) != 1 {
		t.Fatalf("search redis total=%v, want 1", resp["total"])
	}
	got := resp["containers"].([]*dockercli.Container)
	if len(got) != 1 || got[0].ID != "2" {
		t.Errorf("search redis returned %v", ids(got))
	}

	// Search matches stack and service too.
	resp = buildContainerPage(sampleContainers(), "blog", "", 1, 0)
	if resp["total"].(int) != 2 {
		t.Errorf("search blog total=%v, want 2", resp["total"])
	}
}

func TestBuildContainerPageStateFilter(t *testing.T) {
	resp := buildContainerPage(sampleContainers(), "", "running", 1, 0)
	if resp["total"].(int) != 2 {
		t.Errorf("running total=%v, want 2", resp["total"])
	}
	// "stopped" means any non-running state (exited + paused here).
	resp = buildContainerPage(sampleContainers(), "", "stopped", 1, 0)
	if resp["total"].(int) != 2 {
		t.Errorf("stopped total=%v, want 2", resp["total"])
	}
	resp = buildContainerPage(sampleContainers(), "", "paused", 1, 0)
	if resp["total"].(int) != 1 {
		t.Errorf("paused total=%v, want 1", resp["total"])
	}
}

func TestBuildContainerPagePagination(t *testing.T) {
	p1 := buildContainerPage(sampleContainers(), "", "", 1, 2)
	if p1["total"].(int) != 4 {
		t.Fatalf("total=%v, want 4 (pre-pagination)", p1["total"])
	}
	if got := p1["containers"].([]*dockercli.Container); len(got) != 2 {
		t.Fatalf("page1 len=%d, want 2", len(got))
	}
	p2 := buildContainerPage(sampleContainers(), "", "", 2, 2)
	if got := p2["containers"].([]*dockercli.Container); len(got) != 2 {
		t.Fatalf("page2 len=%d, want 2", len(got))
	}
	// Out-of-range page yields an empty slice, not a panic.
	p9 := buildContainerPage(sampleContainers(), "", "", 9, 2)
	if got := p9["containers"].([]*dockercli.Container); len(got) != 0 {
		t.Errorf("page9 len=%d, want 0", len(got))
	}
}

func TestBuildContainerPageNoPagination(t *testing.T) {
	// page_size 0 = backward-compatible full filtered list.
	resp := buildContainerPage(sampleContainers(), "", "", 1, 0)
	if got := resp["containers"].([]*dockercli.Container); len(got) != 4 {
		t.Errorf("no-pagination len=%d, want 4", len(got))
	}
}
