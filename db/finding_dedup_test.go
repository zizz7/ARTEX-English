package db

import "testing"

func TestFindDuplicateFinding(t *testing.T) {
	d, assets, _ := testSetup(t)
	defer d.Close()

	expID, err := d.CreateExploration("dedup test", "goal")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Exec(`DELETE FROM explorations WHERE id=$1`, expID)
	ts := d.Exploration(expID)

	a1, err := assets.UpsertRootDomain(UpsertRootDomainReq{Domain: "dedup-a.example", TaskID: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer deleteAsset(d, a1)
	a2, err := assets.UpsertRootDomain(UpsertRootDomainReq{Domain: "dedup-b.example", TaskID: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer deleteAsset(d, a2)

	payload := map[string]any{"vulnclass": "SQL Injection", "name": "SQLi in id", "severity": "high"}
	first, err := ts.AddNode(KindFinding, payload, 9, "confirmed", "w1", []int64{a2, a1})
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		desc        string
		class, name string
		anchors     []int64
		want        int64
	}{
		{"same class, name and assets (any order, case/space-insensitive)", " sql injection ", "SQLI IN ID", []int64{a1, a2, a1}, first},
		{"different asset set", "SQL Injection", "SQLi in id", []int64{a1}, 0},
		{"no assets", "SQL Injection", "SQLi in id", nil, 0},
		{"different name", "SQL Injection", "SQLi in name", []int64{a1, a2}, 0},
		{"different class", "XSS", "SQLi in id", []int64{a1, a2}, 0},
	}
	for _, c := range cases {
		got, err := ts.FindDuplicateFinding(c.class, c.name, c.anchors)
		if err != nil {
			t.Fatalf("%s: %v", c.desc, err)
		}
		if got != c.want {
			t.Errorf("%s: got %d, want %d", c.desc, got, c.want)
		}
	}

	// another exploration never sees this one's findings
	otherID, err := d.CreateExploration("other", "goal")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Exec(`DELETE FROM explorations WHERE id=$1`, otherID)
	if got, _ := d.Exploration(otherID).FindDuplicateFinding("SQL Injection", "SQLi in id", []int64{a1, a2}); got != 0 {
		t.Errorf("cross-exploration match: got %d", got)
	}
}
