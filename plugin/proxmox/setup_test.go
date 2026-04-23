package proxmox

import "testing"

func TestIncidr(t *testing.T) {
	res, err := incidr("192.168.0.1", "192.168.0.0/24")
	if !res || err != nil {
		t.Errorf("Incidr error: %t, %v, want match for true, <nil>", res, err)
	}

	res, err = incidr("192.168.1.1", "192.168.0.0/24")
	if res || err != nil {
		t.Errorf("Incidr error: %t, %v, want match for false, <nil>", res, err)
	}

	res, err = incidr("fe80::1", "fe80::/10")
	if !res || err != nil {
		t.Errorf("Incidr error: %t, %v, want match for true, <nil>", res, err)
	}

	res, err = incidr("fe80::1%eth0", "fe80::/10")
	if res || err != nil {
		t.Errorf("Incidr error: %t, %v, want match for false, <nil>", res, err)
	}
}
