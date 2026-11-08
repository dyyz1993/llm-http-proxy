package main

import (
	"testing"
	"time"
)

// ---------- isAuthVerificationFailure ----------

func TestIsAuthVerificationFailure(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"z.ai 1000 身份验证失败", `{"error":{"code":"1000","message":"身份验证失败。"}}`, true},
		{"消息含身份验证但无code", `{"error":{"code":"","message":"身份验证失败"}}`, true},
		{"其他语义401(欠费类)", `{"error":{"code":"1113","message":"余额不足"}}`, false},
		{"空body", ``, false},
		{"非JSON但含关键词", `xxx 身份验证失败 xxx`, true},
		{"无关JSON", `{"ok":true}`, false},
	}
	for _, c := range cases {
		if got := isAuthVerificationFailure([]byte(c.body)); got != c.want {
			t.Errorf("%s: isAuthVerificationFailure(%q)=%v, 期望 %v", c.name, c.body, got, c.want)
		}
	}
}

// ---------- markAuth401: 阈值隔离 ----------

func TestMarkAuth401_IsolatesAtThreshold(t *testing.T) {
	ks := newKeyStore()
	ks.configs["dead"] = KeyConfig{Key: "fake-zai-key-1234567890", Header: "Authorization"}
	body := []byte(`{"error":{"code":"1000","message":"身份验证失败。"}}`)

	markAuth401(ks, "dead", body)
	cfg, _ := ks.lookup("dead")
	if cfg.AuthDead {
		t.Fatal("第 1 次 401 不应立即隔离(阈值=2)")
	}

	markAuth401(ks, "dead", body)
	cfg, _ = ks.lookup("dead")
	if !cfg.AuthDead {
		t.Fatal("连续第 2 次 401 应隔离")
	}
	if cfg.AuthDeadAt == "" {
		t.Error("隔离应记录时刻(探测周期起点)")
	}
}

func TestMarkAuth401_NonAuth401NotCounted(t *testing.T) {
	ks := newKeyStore()
	ks.configs["k"] = KeyConfig{Key: "fake-zai-key-1234567890"}
	other := []byte(`{"error":{"code":"1113","message":"余额不足"}}`)

	markAuth401(ks, "k", other)
	markAuth401(ks, "k", other)
	cfg, _ := ks.lookup("k")
	if cfg.AuthDead {
		t.Error("非'身份验证失败'语义的 401 不应计数隔离")
	}
}

func TestMarkAuth401_NoIsolationWithoutConfig(t *testing.T) {
	ks := newKeyStore()
	body := []byte(`{"error":{"code":"1000"}}`)
	markAuth401(ks, "ghost", body) // alias 不存在,不应 panic/写入
	markAuth401(ks, "ghost", body)
	if _, ok := ks.configs["ghost"]; ok {
		t.Error("未知 alias 不应被创建")
	}
}

// ---------- noteAuthSuccess: 清计数 / 自动复活 ----------

func TestNoteAuthSuccess_ClearsCount(t *testing.T) {
	ks := newKeyStore()
	ks.configs["k"] = KeyConfig{Key: "fake-zai-key-1234567890"}
	body := []byte(`{"error":{"code":"1000"}}`)

	markAuth401(ks, "k", body) // 计数=1
	noteAuthSuccess(ks, "k")
	markAuth401(ks, "k", body) // 计数重新从 1 开始
	cfg, _ := ks.lookup("k")
	if cfg.AuthDead {
		t.Error("成功响应后计数应清零,不应被两次非连续 401 隔离")
	}
}

func TestNoteAuthSuccess_RevivesDead(t *testing.T) {
	ks := newKeyStore()
	ks.configs["k"] = KeyConfig{Key: "fake-zai-key-1234567890"}
	body := []byte(`{"error":{"code":"1000"}}`)
	markAuth401(ks, "k", body)
	markAuth401(ks, "k", body) // 隔离

	noteAuthSuccess(ks, "k") // 隔离期间突然成功(账号恢复)
	cfg, _ := ks.lookup("k")
	if cfg.AuthDead {
		t.Error("隔离中收到成功响应应自动复活")
	}
}

// ---------- clearAuthDead: 手动复活 ----------

func TestClearAuthDead(t *testing.T) {
	ks := newKeyStore()
	ks.configs["k"] = KeyConfig{Key: "fake-zai-key-1234567890"}
	body := []byte(`{"error":{"code":"1000"}}`)
	markAuth401(ks, "k", body)
	markAuth401(ks, "k", body)

	if err := clearAuthDead(ks, "k"); err != nil {
		t.Fatalf("手动复活失败: %v", err)
	}
	cfg, _ := ks.lookup("k")
	if cfg.AuthDead || cfg.AuthDeadAt != "" {
		t.Error("复活后标记与时刻应清空")
	}
	// 复活后计数也清零:再来一次 401 不会立即隔离
	markAuth401(ks, "k", body)
	cfg, _ = ks.lookup("k")
	if cfg.AuthDead {
		t.Error("复活后 401 计数应从零开始")
	}
}

// ---------- shouldProbeAuthDead / touchAuthDead ----------

func TestShouldProbeAuthDead(t *testing.T) {
	now := time.Now()
	fresh := KeyConfig{AuthDead: true, AuthDeadAt: now.In(beijing).Format("2006-01-02 15:04:05")}
	if shouldProbeAuthDead(fresh, now) {
		t.Error("刚隔离的 key 不应立即探测(满 24h 才探)")
	}

	old := KeyConfig{AuthDead: true, AuthDeadAt: now.Add(-25 * time.Hour).In(beijing).Format("2006-01-02 15:04:05")}
	if !shouldProbeAuthDead(old, now) {
		t.Error("隔离超 24h 应触发低频探测")
	}

	alive := KeyConfig{AuthDead: false}
	if shouldProbeAuthDead(alive, now) {
		t.Error("未隔离的 key 不需要探测")
	}

	// 手工编辑的时间格式损坏 → 探一次(防永久卡死)
	broken := KeyConfig{AuthDead: true, AuthDeadAt: "not-a-time"}
	if !shouldProbeAuthDead(broken, now) {
		t.Error("时刻解析失败应探测一次")
	}
}

func TestTouchAuthDead_RenewsCycle(t *testing.T) {
	ks := newKeyStore()
	old := time.Now().Add(-25 * time.Hour).In(beijing).Format("2006-01-02 15:04:05")
	ks.configs["k"] = KeyConfig{Key: "fake-zai-key-1234567890", AuthDead: true, AuthDeadAt: old}

	touchAuthDead(ks, "k")
	cfg, _ := ks.lookup("k")
	renewed, err := time.ParseInLocation("2006-01-02 15:04:05", cfg.AuthDeadAt, beijing)
	if err != nil {
		t.Fatalf("刷新后的时刻解析失败: %v", err)
	}
	if time.Since(renewed) > time.Minute {
		t.Errorf("探测失败后应刷新隔离时刻(下一个 24h 从现在起算),得到 %v", cfg.AuthDeadAt)
	}
}

// ---------- computeMemberHealth: auth_dead 标签 ----------

func TestComputeMemberHealth_AuthDead(t *testing.T) {
	ks := newKeyStore()
	ks.configs["k"] = KeyConfig{Key: "fake-zai-key-1234567890", AuthDead: true, AuthDeadAt: "2026-11-08 22:00:00"}
	if got := computeMemberHealth(ks, nil, "k"); got != "auth_dead" {
		t.Errorf("隔离中的 key 健康标签应为 auth_dead,得到 %q", got)
	}
}

// ---------- markAuth401Msg: 配额轮询直连路径 ----------

func TestMarkAuth401Msg_IsolatesAtThreshold(t *testing.T) {
	ks := newKeyStore()
	ks.configs["poll-dead"] = KeyConfig{Key: "fake-zai-key-1234567890"}

	// 模拟配额轮询的 qr.Msg(直连路径,不经过转发钩子)
	markAuth401Msg(ks, "poll-dead", "身份验证失败。")
	cfg, _ := ks.lookup("poll-dead")
	if cfg.AuthDead {
		t.Fatal("第 1 次不应隔离")
	}
	markAuth401Msg(ks, "poll-dead", "身份验证失败。")
	cfg, _ = ks.lookup("poll-dead")
	if !cfg.AuthDead {
		t.Fatal("第 2 次应隔离(轮询盲区修复)")
	}
}

func TestMarkAuth401Msg_NonAuthMsgNotCounted(t *testing.T) {
	ks := newKeyStore()
	ks.configs["k"] = KeyConfig{Key: "fake-zai-key-1234567890"}
	markAuth401Msg(ks, "k", "余额不足")
	markAuth401Msg(ks, "k", "余额不足")
	cfg, _ := ks.lookup("k")
	if cfg.AuthDead {
		t.Error("非身份验证语义的消息不应计数")
	}
}

func TestMarkAuth401_SharedCounter(t *testing.T) {
	// 转发路径和轮询路径共用同一计数器:各来一次也应达到阈值
	ks := newKeyStore()
	ks.configs["k"] = KeyConfig{Key: "fake-zai-key-1234567890"}
	markAuth401(ks, "k", []byte(`{"error":{"code":"1000"}}`))
	markAuth401Msg(ks, "k", "身份验证失败。")
	cfg, _ := ks.lookup("k")
	if !cfg.AuthDead {
		t.Error("两条计数入口应共用计数(1+1=2 达阈值)")
	}
}
