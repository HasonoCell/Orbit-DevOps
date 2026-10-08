package identity

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"strings"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
)

const passwordProfile = "$argon2id$v=19$m=19456,t=2,p=1$"

// hashPassword 在安全事务外工作；有限并发约束单实例的内存与 CPU 消耗。
func (m *Module) hashPassword(ctx context.Context, password string) (string, error) {
	if !validPassword(password) {
		return "", ErrInvalidPassword
	}
	select {
	case m.hashSlots <- struct{}{}:
		defer func() { <-m.hashSlots }()
	case <-ctx.Done():
		return "", ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", ErrUnavailable
	}
	key := argon2.IDKey([]byte(password), salt, 2, 19*1024, 1, 32)
	defer clear(key)
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return passwordProfile + base64.RawStdEncoding.EncodeToString(salt) + "$" + base64.RawStdEncoding.EncodeToString(key), nil
}

// verifyPassword 只接受实现支持的有界 PHC profile，绝不照 DB 中任意参数分配内存。
// 未知/坏 Hash 使用相同成本 dummy，不给未经证明请求暴露凭据存储细节。
func (m *Module) verifyPassword(ctx context.Context, password, encoded string) (bool, error) {
	salt, expected, valid := decodePasswordHash(encoded)
	if !valid {
		salt, expected, _ = decodePasswordHash(m.dummyHash)
	}
	select {
	case m.hashSlots <- struct{}{}:
		defer func() { <-m.hashSlots }()
	case <-ctx.Done():
		return false, ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	key := argon2.IDKey([]byte(password), salt, 2, 19*1024, 1, 32)
	defer clear(key)
	if err := ctx.Err(); err != nil {
		return false, err
	}
	return subtle.ConstantTimeCompare(key, expected) == 1 && valid, nil
}

func decodePasswordHash(encoded string) ([]byte, []byte, bool) {
	if len(encoded) > 512 || !strings.HasPrefix(encoded, passwordProfile) {
		return nil, nil, false
	}
	parts := strings.Split(strings.TrimPrefix(encoded, passwordProfile), "$")
	if len(parts) != 2 {
		return nil, nil, false
	}
	salt, saltErr := base64.RawStdEncoding.Strict().DecodeString(parts[0])
	key, keyErr := base64.RawStdEncoding.Strict().DecodeString(parts[1])
	// Go 的 Strict 解码仍忽略 CR/LF；必须回编码相等，才能与有效入口投影使用同一规范。
	// 不让带换行的存储 Hash 一边能登录、一边被最后管理员/owner 保护视作不存在。
	valid := saltErr == nil && keyErr == nil && len(salt) == 16 && len(key) == 32 &&
		base64.RawStdEncoding.EncodeToString(salt) == parts[0] &&
		base64.RawStdEncoding.EncodeToString(key) == parts[1]
	return salt, key, valid
}

func validPassword(password string) bool {
	if !utf8.ValidString(password) || len(password) > 512 {
		return false
	}
	count := utf8.RuneCountInString(password)
	if count < 12 || count > 128 {
		return false
	}
	// 拒绝常见样例弱口令及单字符重复；不能借弱口令检查 trim/截断实际密码。
	switch strings.ToLower(password) {
	case "passwordpassword", "password123456789", "123456789012345", "1234567890123456",
		"correct horse battery staple", "qwertyuiopasdfgh":
		return false
	}
	var first rune
	for i, c := range password {
		if i == 0 {
			first = c
		} else if c != first {
			return true
		}
	}
	return false
}
