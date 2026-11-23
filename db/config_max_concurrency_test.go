package db

import "testing"

// TestProfileMaxConcurrencyRoundTrip covers every column list that carries
// max_concurrency: insert, keyed load, list load, and both update branches.
func TestProfileMaxConcurrencyRoundTrip(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	t.Cleanup(func() { d.Close() })

	id, err := d.SaveProfile(&LLMProfile{Name: "t-maxconc", Format: "openai", Model: "m", APIKey: "k", MaxConcurrency: 5})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Exec(`DELETE FROM llm_profiles WHERE id=$1`, id) })

	p, err := d.ProfileByID(id)
	if err != nil || p == nil || p.MaxConcurrency != 5 {
		t.Fatalf("keyed load: err=%v max_concurrency=%v", err, p)
	}
	ps, err := d.ListProfiles()
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range ps {
		if x.ID == id && x.MaxConcurrency != 5 {
			t.Fatalf("list load: max_concurrency=%d", x.MaxConcurrency)
		}
	}

	p.APIKey = "" // keep-existing-key UPDATE branch
	p.MaxConcurrency = 4
	if _, err := d.SaveProfile(p); err != nil {
		t.Fatal(err)
	}
	if after, _ := d.ProfileByID(id); after == nil || after.MaxConcurrency != 4 || after.APIKey != "k" {
		t.Fatalf("blank-key update: %+v", after)
	}

	p.APIKey = "k2" // new-key UPDATE branch
	p.MaxConcurrency = 0
	if _, err := d.SaveProfile(p); err != nil {
		t.Fatal(err)
	}
	if after, _ := d.ProfileByID(id); after == nil || after.MaxConcurrency != 0 {
		t.Fatalf("new-key update: %+v", after)
	}

	if _, err := d.Exec(`UPDATE llm_profiles SET max_concurrency=-1 WHERE id=$1`, id); err == nil {
		t.Fatal("negative max_concurrency must be rejected by the CHECK constraint")
	}
}
