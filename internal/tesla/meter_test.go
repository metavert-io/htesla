package tesla

import "testing"

func TestMeterClassifies(t *testing.T) {
	m := &Meter{}
	for _, c := range [][2]string{
		{"GET", "/api/1/vehicles"},
		{"POST", "/api/1/vehicles/fleet_status"},
		{"GET", "/api/1/vehicles/5YJ3E1EA0KF000001/fleet_telemetry_config"},
		{"DELETE", "/api/1/vehicles/5YJ3E1EA0KF000001/fleet_telemetry_config"},
		{"GET", "/api/1/vehicles/5YJ3E1EA0KF000001/release_notes"},
		{"POST", "/api/1/partner_accounts"},
		{"GET", "/api/1/vehicles/5YJ3E1EA0KF000001/vehicle_data"}, // billed (never called)
	} {
		m.Count(c[0], c[1])
	}
	free, billed, by := m.Take()
	if free != 6 || billed != 1 {
		t.Fatalf("free %d billed %d %v", free, billed, by)
	}
	if by["GET /api/1/vehicles/{vin}/vehicle_data"] != 1 {
		t.Fatalf("the VIN must not appear in the meter: %v", by)
	}
	if f, b, _ := m.Take(); f != 0 || b != 0 {
		t.Fatal("Take resets")
	}
}
