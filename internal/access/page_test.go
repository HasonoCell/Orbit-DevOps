package access

import "testing"

func TestAccessPageIsBoundedAndDefaults(t *testing.T) {
	page, err := (Page{}).normalized()
	if err != nil || page.Limit != 50 || page.Offset != 0 {
		t.Fatalf("unexpected default page: %+v, %v", page, err)
	}
	for _, input := range []Page{{Limit: -1}, {Limit: 101}, {Offset: -1}, {Offset: 10001}} {
		if _, err := input.normalized(); err == nil {
			t.Fatalf("accepted unbounded page: %+v", input)
		}
	}
}
