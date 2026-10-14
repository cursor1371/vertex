package config

type ConfigProvider interface {
	PortAPI() int
	MaxRetries() int
	AdminPassword() string
	DebugPprof() bool
	DebugMode() bool
	AdminAPILogEnabled() bool
	TrailingModelFixEnabled() bool
	TrailingFixModels() []string

	DropMaxTokens() bool

	AggregateStream() bool
	MaxN() int
	MaxRequestMB() int
	RequestTimeoutSeconds() int
	RequestTotalTimeoutSeconds() int
	MaxSpillMB() int

	VertexAPIKey() string
	CountTokensQuerySignature() string

	SafetySettings() map[string]string

	ParallelPoolEnabled() bool
	ParallelPoolSize() int
	MaxConcurrent() int
	RTCompensationMs() int
	RecaptchaTokenPrefetch() bool
	RecaptchaPrefetchDepth() int
	RecaptchaPrefetchTTLSeconds() int
	RecaptchaPrefetchAdaptive() bool
	RecaptchaTryEntryOrDirect() bool
	RecaptchaNoDirectFetch() bool
	RecaptchaPrefetchBudgetSeconds() int
	RecaptchaLiveBudgetSeconds() int
	ActiveNodeURI() string

	BackgroundImage() string
	FontSize() string
	FontColorType() string
	FontColor() string
	CustomBgPresets() []string
	AutoRefreshLogs() bool

	BaseModels() []string
	AliasMap() map[string]string
	ModelsWithVariants() []string
	FakePrefixes() []string
	AntiTruncatePrefixes() []string
	ResolveModelName(string) string

	ConfigDir() string
	ConfigPath() string

	DefaultImageSize() string
	DefaultThinkingLevel() string
	DefaultResponseModalities() string
	StreamIdleTimeoutSeconds() int

	ForceNodeIPv6() bool
	HostIPv6Interface() string
	HostIPv6Address() string

	HistoryRetentionDays() int
	RaceLogRetentionDays() int

	HTTPLogEnabled() bool
	HTTPLogRetentionDays() int
	HTTPLogBodyCapKB() int

	ForcedToolCallMode() string
	ForcedToolCallModels() []string

	OpenAISSEHeartbeatSeconds() int

	HealthProbeFailThreshold() int
	HealthSelfHealEnabled() bool
	HealthSelfHealIntervalMinutes() int
	HealthSelfHealProbeConcurrency() int
	HealthGlobalFaultGuardEnabled() bool
	HealthGlobalFaultMinNodes() int
	HealthGlobalFaultWindowSeconds() int

	BrowserFingerprintEnabled() bool
	BrowserFingerprintMode() string
	BrowserFingerprintPreset() string
	BrowserFingerprintCustom() string
	BrowserFingerprintTLSMode() string
	BrowserFingerprintTLSProfile() string
	BrowserFingerprintTLSMin() int
	BrowserFingerprintTLSMax() int
	BrowserFingerprintTLSPool() []string
	BrowserFingerprintLocale() string
}

type ConfigWriter interface {
	WriteSettings(map[string]any) error
	WriteModels([]string, map[string]string, map[string]string) error
}

type dynamicConfig struct{}

func GetProvider() ConfigProvider { return dynamicConfig{} }

func (d dynamicConfig) PortAPI() int                  { return Load().PortAPI }
func (d dynamicConfig) MaxRetries() int               { return Load().MaxRetries }
func (d dynamicConfig) AdminPassword() string         { return Load().AdminPassword }
func (d dynamicConfig) DebugPprof() bool              { return Load().DebugPprof }
func (d dynamicConfig) DebugMode() bool               { return Load().DebugMode }
func (d dynamicConfig) AdminAPILogEnabled() bool      { return Load().AdminAPILogEnabled }
func (d dynamicConfig) TrailingModelFixEnabled() bool { return Load().TrailingModelFixEnabled }
func (d dynamicConfig) TrailingFixModels() []string {
	c := Load()
	out := make([]string, len(c.TrailingFixModels))
	copy(out, c.TrailingFixModels)
	return out
}
func (d dynamicConfig) DropMaxTokens() bool               { return Load().DropMaxTokens }
func (d dynamicConfig) AggregateStream() bool             { return Load().AggregateStream }
func (d dynamicConfig) MaxN() int                         { return Load().MaxN }
func (d dynamicConfig) MaxRequestMB() int                 { return Load().MaxRequestMB }
func (d dynamicConfig) RequestTimeoutSeconds() int        { return Load().RequestTimeoutSeconds }
func (d dynamicConfig) RequestTotalTimeoutSeconds() int   { return Load().RequestTotalTimeoutSeconds }
func (d dynamicConfig) MaxSpillMB() int                   { return Load().MaxSpillMB }
func (d dynamicConfig) VertexAPIKey() string              { return Load().VertexAPIKey }
func (d dynamicConfig) CountTokensQuerySignature() string { return Load().CountTokensQuerySignature }
func (d dynamicConfig) SafetySettings() map[string]string {
	c := Load()
	out := make(map[string]string, len(c.SafetySettings))
	for k, v := range c.SafetySettings {
		out[k] = v
	}
	return out
}
func (d dynamicConfig) ParallelPoolEnabled() bool { return Load().ParallelPoolEnabled }
func (d dynamicConfig) ParallelPoolSize() int     { return Load().ParallelPoolSize }
func (d dynamicConfig) MaxConcurrent() int        { return Load().MaxConcurrent }
func (d dynamicConfig) RTCompensationMs() int     { return Load().RTCompensationMs }
func (d dynamicConfig) RecaptchaTokenPrefetch() bool {
	return Load().RecaptchaTokenPrefetch
}
func (d dynamicConfig) RecaptchaPrefetchDepth() int      { return Load().RecaptchaPrefetchDepth }
func (d dynamicConfig) RecaptchaPrefetchTTLSeconds() int { return Load().RecaptchaPrefetchTTLSec }
func (d dynamicConfig) RecaptchaPrefetchAdaptive() bool  { return Load().RecaptchaPrefetchAdaptive }
func (d dynamicConfig) RecaptchaTryEntryOrDirect() bool  { return Load().RecaptchaTryEntryOrDirect }
func (d dynamicConfig) RecaptchaNoDirectFetch() bool     { return Load().RecaptchaNoDirectFetch }
func (d dynamicConfig) RecaptchaPrefetchBudgetSeconds() int {
	return Load().RecaptchaPrefetchBudgetSec
}
func (d dynamicConfig) RecaptchaLiveBudgetSeconds() int { return Load().RecaptchaLiveBudgetSec }
func (d dynamicConfig) ActiveNodeURI() string           { return Load().ActiveNodeURI }
func (d dynamicConfig) BackgroundImage() string         { return Load().BackgroundImage }
func (d dynamicConfig) FontSize() string                { return Load().FontSize }
func (d dynamicConfig) FontColorType() string           { return Load().FontColorType }
func (d dynamicConfig) FontColor() string               { return Load().FontColor }
func (d dynamicConfig) CustomBgPresets() []string {
	c := Load()
	out := make([]string, len(c.CustomBgPresets))
	copy(out, c.CustomBgPresets)
	return out
}
func (d dynamicConfig) BaseModels() []string              { return Load().BaseModels() }
func (d dynamicConfig) AliasMap() map[string]string       { return Load().AliasMap() }
func (d dynamicConfig) ModelsWithVariants() []string      { return Load().ModelsWithVariants() }
func (d dynamicConfig) FakePrefixes() []string            { return Load().FakePrefixes() }
func (d dynamicConfig) AntiTruncatePrefixes() []string    { return Load().AntiTruncatePrefixes() }
func (d dynamicConfig) ResolveModelName(s string) string  { return Load().ResolveModelName(s) }
func (d dynamicConfig) AutoRefreshLogs() bool             { return Load().GetAutoRefreshLogs() }
func (d dynamicConfig) DefaultImageSize() string          { return Load().DefaultImageSize }
func (d dynamicConfig) DefaultThinkingLevel() string      { return Load().DefaultThinkingLevel }
func (d dynamicConfig) DefaultResponseModalities() string { return Load().DefaultResponseModalities }
func (d dynamicConfig) StreamIdleTimeoutSeconds() int     { return Load().StreamIdleTimeoutSeconds }
func (d dynamicConfig) ForceNodeIPv6() bool               { return Load().ForceNodeIPv6 }
func (d dynamicConfig) HostIPv6Interface() string         { return Load().HostIPv6Interface }
func (d dynamicConfig) HostIPv6Address() string           { return Load().HostIPv6Address }
func (d dynamicConfig) HistoryRetentionDays() int         { return Load().HistoryRetentionDays }
func (d dynamicConfig) RaceLogRetentionDays() int         { return Load().RaceLogRetentionDays }
func (d dynamicConfig) HTTPLogEnabled() bool              { return Load().HTTPLogEnabled }
func (d dynamicConfig) HTTPLogRetentionDays() int         { return Load().HTTPLogRetentionDays }
func (d dynamicConfig) HTTPLogBodyCapKB() int             { return Load().HTTPLogBodyCapKB }
func (d dynamicConfig) ForcedToolCallMode() string        { return Load().ForcedToolCallMode }
func (d dynamicConfig) ForcedToolCallModels() []string {
	c := Load()
	out := make([]string, len(c.ForcedToolCallModels))
	copy(out, c.ForcedToolCallModels)
	return out
}
func (d dynamicConfig) ConfigDir() string              { return Load().ConfigDir() }
func (d dynamicConfig) ConfigPath() string             { return Load().ConfigPath() }
func (d dynamicConfig) OpenAISSEHeartbeatSeconds() int { return Load().OpenAISSEHeartbeatSeconds }

func (d dynamicConfig) HealthProbeFailThreshold() int { return Load().HealthProbeFailThreshold() }
func (d dynamicConfig) HealthSelfHealEnabled() bool   { return Load().HealthSelfHealEnabled }
func (d dynamicConfig) HealthSelfHealIntervalMinutes() int {
	return Load().HealthSelfHealIntervalMinutes
}
func (d dynamicConfig) HealthSelfHealProbeConcurrency() int {
	return Load().HealthSelfHealProbeConcurrency
}
func (d dynamicConfig) HealthGlobalFaultGuardEnabled() bool {
	return Load().HealthGlobalFaultGuardEnabled
}
func (d dynamicConfig) HealthGlobalFaultMinNodes() int { return Load().HealthGlobalFaultMinNodes }
func (d dynamicConfig) HealthGlobalFaultWindowSeconds() int {
	return Load().HealthGlobalFaultWindowSeconds
}

func (d dynamicConfig) BrowserFingerprintEnabled() bool { return Load().BrowserFingerprintEnabled }
func (d dynamicConfig) BrowserFingerprintMode() string  { return Load().BrowserFingerprintMode }
func (d dynamicConfig) BrowserFingerprintPreset() string {
	return Load().BrowserFingerprintPreset
}
func (d dynamicConfig) BrowserFingerprintCustom() string { return Load().BrowserFingerprintCustom }
func (d dynamicConfig) BrowserFingerprintTLSMode() string {
	return Load().BrowserFingerprintTLSMode
}
func (d dynamicConfig) BrowserFingerprintTLSProfile() string {
	return Load().BrowserFingerprintTLSProfile
}
func (d dynamicConfig) BrowserFingerprintTLSMin() int    { return Load().BrowserFingerprintTLSMin }
func (d dynamicConfig) BrowserFingerprintTLSMax() int    { return Load().BrowserFingerprintTLSMax }
func (d dynamicConfig) BrowserFingerprintLocale() string { return Load().BrowserFingerprintLocale }
func (d dynamicConfig) BrowserFingerprintTLSPool() []string {
	c := Load()
	out := make([]string, len(c.BrowserFingerprintTLSPool))
	copy(out, c.BrowserFingerprintTLSPool)
	return out
}
