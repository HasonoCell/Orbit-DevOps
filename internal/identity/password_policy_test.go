package identity

import (
	"strings"
	"testing"
)

// 策略按 Unicode 字符而非字节计数；缩短下限不能放宽弱口令和上限检查。
func TestPasswordPolicyBoundaries(t *testing.T) {
	tests := []struct {
		name     string
		password string
		valid    bool
	}{
		{"eleven characters", "Abcdef12345", false},
		{"twelve characters", "Abcdef123456", true},
		{"eleven unicode characters", strings.Repeat("🔒", 10) + "A", false},
		{"twelve unicode characters", strings.Repeat("🔒", 11) + "A", true},
		{"maximum characters", strings.Repeat("🔒", 127) + "界", true},
		{"too many characters", strings.Repeat("a", 128) + "b", false},
		{"too many bytes", strings.Repeat("🔒", 128) + "A", false},
		{"invalid utf8", "Abcdef123456\xff", false},
		{"blocked password", "PASSWORDPASSWORD", false},
		{"repeated character", strings.Repeat("a", 12), false},
		{"repeated unicode character", strings.Repeat("🔒", 12), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := validPassword(tt.password); got != tt.valid {
				t.Fatalf("validPassword() = %v, want %v", got, tt.valid)
			}
		})
	}
}
