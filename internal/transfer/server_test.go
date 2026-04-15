package transfer

import "testing"

func TestParseIBRateMbps(t *testing.T) {
	cases := []struct {
		in   string
		want int64
	}{
		// NDR (400 Gbps per port)
		{"400 Gb/s (4X NDR)", 400_000},
		// HDR (200 Gbps per port)
		{"200 Gb/s (4X HDR)", 200_000},
		{"200 Gb/s", 200_000},
		// EDR
		{"100 Gb/s (4X EDR)", 100_000},
		// FDR
		{"56 Gb/s (4X FDR)", 56_000},
		// QDR
		{"40 Gb/s (4X QDR)", 40_000},
		// DDR
		{"20 Gb/s (4X DDR)", 20_000},
		// SDR
		{"10 Gb/s (4X SDR)", 10_000},
		{"2.5 Gb/s (1X SDR)", 2_500},
		// Mellanox "down/unpopulated" port — bytes/sec
		{"0 GB/sec", 0},
		{"100 GB/sec", 800_000}, // 100 GB/s × 8 = 800 Gbps
		// Trailing newline (real sysfs output)
		{"200 Gb/s (4X HDR)\n", 200_000},
		// Empty / garbage
		{"", 0},
		{"unknown", 0},
		{"-1", 0},
	}
	for _, tc := range cases {
		got := parseIBRateMbps(tc.in)
		if got != tc.want {
			t.Errorf("parseIBRateMbps(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}
