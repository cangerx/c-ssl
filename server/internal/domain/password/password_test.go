package password

import (
	"strings"
	"testing"
)

func TestHashAndVerify(t *testing.T) {
	const plain = "correct-horse-battery"

	encoded, err := Hash(plain)
	if err != nil {
		t.Fatalf("Hash 失败: %v", err)
	}

	// 哈希串必须自描述：算法、版本、参数、盐都在里面
	for _, want := range []string{"$argon2id$", "v=19", "m=", "t=", "p="} {
		if !strings.Contains(encoded, want) {
			t.Errorf("哈希串缺少 %q，实际为 %q", want, encoded)
		}
	}

	if strings.Contains(encoded, plain) {
		t.Fatal("哈希串里出现了明文口令")
	}

	ok, err := Verify(plain, encoded)
	if err != nil {
		t.Fatalf("Verify 出错: %v", err)
	}
	if !ok {
		t.Error("正确口令应当校验通过")
	}
}

func TestVerifyRejectsWrongPassword(t *testing.T) {
	encoded, err := Hash("correct-horse-battery")
	if err != nil {
		t.Fatalf("Hash 失败: %v", err)
	}

	ok, err := Verify("wrong-horse-battery", encoded)
	if err != nil {
		t.Fatalf("Verify 出错: %v", err)
	}
	if ok {
		t.Error("错误口令不应通过校验")
	}
}

// 同一个口令两次哈希必须得到不同结果，否则说明盐没起作用。
func TestHashIsSalted(t *testing.T) {
	const plain = "correct-horse-battery"

	first, err := Hash(plain)
	if err != nil {
		t.Fatalf("第一次 Hash 失败: %v", err)
	}
	second, err := Hash(plain)
	if err != nil {
		t.Fatalf("第二次 Hash 失败: %v", err)
	}

	if first == second {
		t.Error("两次哈希结果相同，盐未生效")
	}

	// 但两者都必须能校验通过
	for i, encoded := range []string{first, second} {
		ok, err := Verify(plain, encoded)
		if err != nil || !ok {
			t.Errorf("第 %d 个哈希校验失败: ok=%v err=%v", i+1, ok, err)
		}
	}
}

func TestVerifyRejectsMalformedHash(t *testing.T) {
	cases := map[string]string{
		"空串":            "",
		"段数不足":          "$argon2id$v=19$m=65536,t=3,p=4$c2FsdA",
		"算法不对":          "$argon2i$v=19$m=65536,t=3,p=4$c2FsdA$aGFzaA",
		"版本不支持":         "$argon2id$v=13$m=65536,t=3,p=4$c2FsdA$aGFzaA",
		"参数格式错":         "$argon2id$v=19$m=x,t=3,p=4$c2FsdA$aGFzaA",
		"盐不是合法 base64":  "$argon2id$v=19$m=65536,t=3,p=4$!!!$aGFzaA",
		"哈希不是合法 base64": "$argon2id$v=19$m=65536,t=3,p=4$c2FsdA$!!!",
		"参数为零":          "$argon2id$v=19$m=0,t=0,p=0$c2FsdA$aGFzaA",
	}

	for name, encoded := range cases {
		t.Run(name, func(t *testing.T) {
			ok, err := Verify("correct-horse-battery", encoded)
			if err == nil {
				t.Errorf("期望返回解析错误，实际 ok=%v err=nil", ok)
			}
			if ok {
				t.Error("解析失败时不应判定为校验通过")
			}
		})
	}
}

func TestValidatePlain(t *testing.T) {
	cases := []struct {
		name    string
		plain   string
		wantErr bool
	}{
		{"空口令", "", true},
		{"太短", "1234567", true},
		{"刚好最短", "12345678", false},
		{"正常长度", "correct-horse-battery", false},
		{"刚好 72 字节", strings.Repeat("a", 72), false},
		{"超过 72 字节", strings.Repeat("a", 73), true},
		// 按字节而非字符判断：24 个汉字是 72 字节，刚好放行
		{"24 个汉字", strings.Repeat("密", 24), false},
		{"25 个汉字", strings.Repeat("密", 25), true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidatePlain(tc.plain)
			if tc.wantErr && err == nil {
				t.Error("期望报错，实际通过")
			}
			if !tc.wantErr && err != nil {
				t.Errorf("期望通过，实际报错: %v", err)
			}
		})
	}
}

func TestHashRejectsInvalidPlain(t *testing.T) {
	if _, err := Hash("short"); err == nil {
		t.Error("过短口令应当拒绝哈希")
	}
	if _, err := Hash(strings.Repeat("a", 100)); err == nil {
		t.Error("过长口令应当拒绝哈希")
	}
}

// 参数未变时不应触发重算，避免每次登录都白白多算一次哈希。
func TestNeedsRehash(t *testing.T) {
	encoded, err := Hash("correct-horse-battery")
	if err != nil {
		t.Fatalf("Hash 失败: %v", err)
	}
	if NeedsRehash(encoded) {
		t.Error("刚生成的哈希不应需要重算")
	}

	if !NeedsRehash("") {
		t.Error("空哈希串应当需要重算")
	}
	if !NeedsRehash("$argon2id$v=19$m=1024,t=1,p=1$c2FsdA$aGFzaA") {
		t.Error("参数不同的哈希应当需要重算")
	}
}

// 低参数哈希仍应能校验通过——这正是把参数写进哈希串的意义。
func TestVerifyAcceptsOlderParams(t *testing.T) {
	// 用低参数生成一条"旧"哈希：m=1024, t=1, p=1
	old := encode(params{memory: 1024, iterations: 1, threads: 1},
		[]byte("0123456789abcdef"), []byte("0123456789abcdef0123456789abcdef"))

	ok, err := Verify("correct-horse-battery", old)
	if err != nil {
		t.Fatalf("校验旧参数哈希出错: %v", err)
	}
	// 这条哈希的密钥是我们随便填的，因此应当校验失败，但不该报解析错误
	if ok {
		t.Error("伪造的哈希不应校验通过")
	}
	if !NeedsRehash(old) {
		t.Error("旧参数的哈希应当需要重算")
	}
}
