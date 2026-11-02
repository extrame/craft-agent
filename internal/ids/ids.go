// Package ids 生成会话这类「给人和日志看」的短 id。
package ids

import (
	"crypto/rand"
	"strconv"
	"time"
)

// alphabet 是 id 的字符集：62 个大小写字母加数字，肉眼可辨、可手抄进日志。
const alphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

// New 返回 8 位随机 id（a-zA-Z0-9）。
//
// 不用 pid：pid 会复用（昨天那个 4321 和今天的 4321 是两个会话），还把进程号
// 这种无意义的东西带到界面上。8 位随机在 62^8 ≈ 2×10^14 的空间里，会话量级下
// 碰撞可以忽略；真撞了也只是两条流水并成一个条目，不是数据事故。
func New() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		// 系统熵源不可用几乎不会发生；真发生了，退回时间戳也总比空 id 强。
		return "s" + strconv.FormatInt(time.Now().UnixNano(), 36)[:7]
	}
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)] // mod 62 有约 3% 偏差，标签用途无所谓
	}
	return string(b)
}
