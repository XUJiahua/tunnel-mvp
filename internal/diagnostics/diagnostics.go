// Package diagnostics preserves compact Agent error codes and correlation IDs
// within remotedialer's 100-byte error limit.
package diagnostics

import (
	"crypto/rand"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"net"
	"net/url"
	"strings"
	"unicode/utf8"
)

type Attempt struct {
	IP      string `json:"ip"`
	Allowed bool   `json:"allowed"`
	Error   string `json:"error,omitempty"`
}
type Failure struct {
	Code           string    `json:"code"`
	Stage          string    `json:"stage"`
	Message        string    `json:"message"`
	Hint           string    `json:"hint"`
	AgentRequestID string    `json:"agentRequestID,omitempty"`
	Target         string    `json:"target,omitempty"`
	ResolvedIPs    []string  `json:"resolvedIPs,omitempty"`
	Attempts       []Attempt `json:"attempts,omitempty"`
}

const marker = "tunnel1 "

func ID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}
func (f *Failure) Error() string {
	// remotedialer reads at most 100 bytes from an error frame. Put the code and
	// correlation ID first; full diagnostics travel over an authenticated POST.
	text := marker + f.Code + " " + f.Stage + " " + f.AgentRequestID + " " + f.Message
	for len(text) > 100 {
		text = text[:len(text)-1]
	}
	for !utf8.ValidString(text) {
		text = text[:len(text)-1]
	}
	return text
}
func New(code, stage, message string) *Failure {
	return &Failure{Code: code, Stage: stage, Message: message, Hint: Hint(code)}
}
func Hint(code string) string {
	switch code {
	case "acl_denied":
		return "检查 Agent allow 中的 CIDR、host 和端口。localhost 可能解析到 ::1；只在确有需要时显式允许 ::1/128。"
	case "dns_failed":
		return "在 Agent 所在机器检查域名解析；目标域名由 Agent 解析。"
	case "connection_refused":
		return "确认应用已启动，核对端口和监听地址。容器内的 127.0.0.1 指向容器自身。"
	case "network_unreachable":
		return "检查 Agent 到目标 IP 的路由、防火墙和容器网络。"
	case "tls_failed":
		return "核对 HTTP/HTTPS、证书主机名和系统 CA 信任；内网应用的 TLS 由 Server 校验。"
	case "timeout":
		return "检查 Agent 网络、目标服务响应和防火墙；对照两端日志中的目标地址与请求 ID。"
	case "site_offline":
		return "确认 Agent 已连接正确的 Server 和 siteID，并检查令牌与重连日志。"
	case "invalid_target":
		return "使用完整 HTTP/HTTPS 源地址并核对主机名和端口。"
	case "http_failed":
		return "TCP 连接之后未收到有效 HTTP 响应；检查应用协议、HTTP/HTTPS 和服务日志。"
	default:
		return "查看 Server 和 Agent 日志中的请求 ID、目标地址及原始网络错误。"
	}
}
func Describe(err error) *Failure {
	var f *Failure
	if errors.As(err, &f) {
		copy := *f
		return &copy
	}
	text := err.Error()
	if start := strings.Index(text, marker); start >= 0 {
		fields := strings.SplitN(text[start:], " ", 5)
		if len(fields) == 5 && len(fields[3]) == 16 {
			if id, err := hex.DecodeString(fields[3]); err == nil && len(id) == 8 {
				remote := New(fields[1], fields[2], fields[4])
				remote.AgentRequestID = fields[3]
				return remote
			}
		}
	}
	// Avoid copying full request URLs (including query parameters) into diagnostics.
	for {
		var u *url.Error
		if !errors.As(err, &u) {
			break
		}
		err = u.Err
	}
	message := err.Error()
	lower := strings.ToLower(message)
	var dns *net.DNSError
	var unknown x509.UnknownAuthorityError
	var invalid x509.CertificateInvalidError
	var hostname x509.HostnameError
	switch {
	case errors.As(err, &dns), strings.Contains(lower, "no such host"), strings.Contains(lower, "name resolution"):
		return New("dns_failed", "dns", message)
	case errors.As(err, &unknown), errors.As(err, &invalid), errors.As(err, &hostname), strings.Contains(lower, "tls"), strings.Contains(lower, "x509"), strings.Contains(lower, "http response to https"):
		return New("tls_failed", "tls", message)
	case strings.Contains(lower, "target denied by agent acl"):
		return New("acl_denied", "acl", message)
	case strings.Contains(lower, "connection refused"), strings.Contains(lower, "actively refused"):
		return New("connection_refused", "tcp", message)
	case strings.Contains(lower, "network is unreachable"), strings.Contains(lower, "no route to host"):
		return New("network_unreachable", "tcp", message)
	case strings.Contains(lower, "timeout"), strings.Contains(lower, "timed out"), strings.Contains(lower, "deadline exceeded"):
		return New("timeout", "http", message)
	default:
		return New("http_failed", "http", message)
	}
}
