package gateway

import (
	"net/netip"
	"slices"
)

// CLIService 为固定CLI构造模型服务；可信Host显式提供源站、凭证引用和出站网段。
func CLIService(agent, origin, credentialRef string, allowedNetworks []netip.Prefix) (Service, error) {
	s := Service{Ref: "model", Origin: origin, CredentialRef: credentialRef, AllowedNetworks: slices.Clone(allowedNetworks)}
	switch agent {
	case "codex":
		s.Paths = []string{"/v1/responses"}
		s.Auth = Bearer
	case "claude":
		s.Paths = []string{"/v1/messages", "/v1/messages?beta=true"}
		s.Auth = APIKey
	default:
		return Service{}, ErrInvalidConfig
	}
	return s, nil
}
