package config

import "testing"

func TestFileLimitsDefaultAndLocalPolicy(t *testing.T) {
	c, err := Parse("[hub]\nsetup=true\n[hub.files]\ncopy_max_bytes=1024\ncopy_max_entries=20\nmax_page_size=10\nbatch_max_items=5\n")
	if err != nil || c.Hub.Files.CopyMaxBytes != 1024 || c.Hub.Files.DefaultPageSize != 10 || c.Hub.Files.BatchMaxItems != 5 {
		t.Fatal(c, err)
	}
	for _, doc := range []string{"copy_max_bytes=-1", "copy_max_entries=10001", "batch_max_items=101", "upload_max_bytes=1099511627777", "max_page_size=201", "max_entries=3", "search_max_examined=1"} {
		if _, err := Parse("[hub]\nsetup=true\n[hub.files]\n" + doc + "\n"); err == nil {
			t.Fatal("invalid local limits", doc)
		}
	}
}

func TestLargeNASBudgetsRequireLocalConfigurationAndStayFinite(t *testing.T) {
	c, e := Parse("[hub]\nsetup=true\n[hub.files]\nupload_max_bytes=34359738368\ncopy_max_bytes=34359738368\n")
	if e != nil || c.Hub.Files.UploadMaxBytes != 32<<30 || c.Hub.Files.CopyMaxBytes != 32<<30 {
		t.Fatal(c, e)
	}
	if DefaultFileLimits().CopyMaxBytes != 8<<30 || DefaultFileLimits().UploadMaxBytes != 8<<30 {
		t.Fatal("large ceiling changed default policy")
	}
	if _, e = Parse("[hub]\nsetup=true\n[hub.files]\ncopy_max_bytes=1099511627777\n"); e == nil {
		t.Fatal("unbounded local byte policy")
	}
}
