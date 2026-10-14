package config

type staticConfig struct {
	c AppConfig
}

func StaticProvider(c AppConfig) ConfigProvider { return staticConfig{c} }

func (s staticConfig) PortAPI() int                  { return s.c.PortAPI }
func (s staticConfig) MaxRetries() int               { return s.c.MaxRetries }
func (s staticConfig) AdminPassword() string         { return s.c.AdminPassword }
func (s staticConfig) DebugPprof() bool              { return s.c.DebugPprof }
func (s staticConfig) DebugMode() bool               { return s.c.DebugMode }
func (s staticConfig) AdminAPILogEnabled() bool      { return s.c.AdminAPILogEnabled }
func (s staticConfig) TrailingModelFixEnabled() bool { return s.c.TrailingModelFixEnabled }
func (s staticConfig) TrailingFixModels() []string {
	out := make([]string, len(s.c.TrailingFixModels))
	copy(out, s.c.TrailingFixModels)
	return out
}
func (s staticConfig) DropMaxTokens() bool               { return s.c.DropMaxTokens }
func (s staticConfig) AggregateStream() bool             { return s.c.AggregateStream }
func (s staticConfig) MaxN() int                         { return s.c.MaxN }
func (s staticConfig) MaxRequestMB() int                 { return s.c.MaxRequestMB }
func (s staticConfig) RequestTimeoutSeconds() int        { return s.c.RequestTimeoutSeconds }
func (s staticConfig) RequestTotalTimeoutSeconds() int   { return s.c.RequestTotalTimeoutSeconds }
func (s staticConfig) MaxSpillMB() int                   { return s.c.MaxSpillMB }
func (s staticConfig) VertexAPIKey() string              { return s.c.VertexAPIKey }
func (s staticConfig) CountTokensQuerySignature() string { return s.c.CountTokensQuerySignature }
func (s staticConfig) SafetySettings() map[string]string { return s.c.SafetySettings }
func (s staticConfig) ParallelPoolEnabled() bool         { return s.c.ParallelPoolEnabled }
func (s staticConfig) ParallelPoolSize() int             { return s.c.ParallelPoolSize }
func (s staticConfig) MaxConcurrent() int                { return s.c.MaxConcurrent }
func (s staticConfig) RTCompensationMs() int             { return s.c.RTCompensationMs }
func (s staticConfig) RecaptchaTokenPrefetch() bool      { return s.c.RecaptchaTokenPrefetch }
func (s staticConfig) RecaptchaPrefetchDepth() int       { return s.c.RecaptchaPrefetchDepth }
func (s staticConfig) RecaptchaPrefetchTTLSeconds() int  { return s.c.RecaptchaPrefetchTTLSec }
func (s staticConfig) RecaptchaPrefetchAdaptive() bool   { return s.c.RecaptchaPrefetchAdaptive }
func (s staticConfig) RecaptchaTryEntryOrDirect() bool   { return s.c.RecaptchaTryEntryOrDirect }
func (s staticConfig) RecaptchaNoDirectFetch() bool      { return s.c.RecaptchaNoDirectFetch }
func (s staticConfig) RecaptchaPrefetchBudgetSeconds() int {
	return s.c.RecaptchaPrefetchBudgetSec
}
func (s staticConfig) RecaptchaLiveBudgetSeconds() int   { return s.c.RecaptchaLiveBudgetSec }
func (s staticConfig) ActiveNodeURI() string             { return s.c.ActiveNodeURI }
func (s staticConfig) BackgroundImage() string           { return s.c.BackgroundImage }
func (s staticConfig) FontSize() string                  { return s.c.FontSize }
func (s staticConfig) FontColorType() string             { return s.c.FontColorType }
func (s staticConfig) FontColor() string                 { return s.c.FontColor }
func (s staticConfig) CustomBgPresets() []string         { return s.c.CustomBgPresets }
func (s staticConfig) AutoRefreshLogs() bool             { return s.c.GetAutoRefreshLogs() }
func (s staticConfig) BaseModels() []string              { return s.c.BaseModels() }
func (s staticConfig) AliasMap() map[string]string       { return s.c.AliasMap() }
func (s staticConfig) ModelsWithVariants() []string      { return s.c.ModelsWithVariants() }
func (s staticConfig) FakePrefixes() []string            { return s.c.FakePrefixes() }
func (s staticConfig) AntiTruncatePrefixes() []string    { return s.c.AntiTruncatePrefixes() }
func (s staticConfig) ResolveModelName(s_ string) string { return s.c.ResolveModelName(s_) }
func (s staticConfig) DefaultImageSize() string          { return s.c.DefaultImageSize }
func (s staticConfig) DefaultThinkingLevel() string      { return s.c.DefaultThinkingLevel }
func (s staticConfig) DefaultResponseModalities() string { return s.c.DefaultResponseModalities }
func (s staticConfig) StreamIdleTimeoutSeconds() int     { return s.c.StreamIdleTimeoutSeconds }
func (s staticConfig) ForceNodeIPv6() bool               { return s.c.ForceNodeIPv6 }
func (s staticConfig) HostIPv6Interface() string         { return s.c.HostIPv6Interface }
func (s staticConfig) HostIPv6Address() string           { return s.c.HostIPv6Address }
func (s staticConfig) HistoryRetentionDays() int         { return s.c.HistoryRetentionDays }
func (s staticConfig) RaceLogRetentionDays() int         { return s.c.RaceLogRetentionDays }
func (s staticConfig) HTTPLogEnabled() bool              { return s.c.HTTPLogEnabled }
func (s staticConfig) HTTPLogRetentionDays() int         { return s.c.HTTPLogRetentionDays }
func (s staticConfig) HTTPLogBodyCapKB() int             { return s.c.HTTPLogBodyCapKB }
func (s staticConfig) ForcedToolCallMode() string        { return s.c.ForcedToolCallMode }
func (s staticConfig) ForcedToolCallModels() []string {
	out := make([]string, len(s.c.ForcedToolCallModels))
	copy(out, s.c.ForcedToolCallModels)
	return out
}
func (s staticConfig) ConfigDir() string  { return s.c.ConfigDir() }
func (s staticConfig) ConfigPath() string { return s.c.ConfigPath() }
func (s staticConfig) OpenAISSEHeartbeatSeconds() int {
	return s.c.OpenAISSEHeartbeatSeconds
}
func (s staticConfig) HealthSelfHealProbeConcurrency() int {
	return s.c.HealthSelfHealProbeConcurrency
}
func (s staticConfig) HealthGlobalFaultGuardEnabled() bool {
	return s.c.HealthGlobalFaultGuardEnabled
}
func (s staticConfig) HealthGlobalFaultMinNodes() int { return s.c.HealthGlobalFaultMinNodes }
func (s staticConfig) HealthGlobalFaultWindowSeconds() int {
	return s.c.HealthGlobalFaultWindowSeconds
}

func (s staticConfig) HealthProbeFailThreshold() int { return s.c.HealthProbeFailThreshold() }
func (s staticConfig) HealthSelfHealEnabled() bool   { return s.c.HealthSelfHealEnabled }
func (s staticConfig) HealthSelfHealIntervalMinutes() int {
	return s.c.HealthSelfHealIntervalMinutes
}

func (s staticConfig) BrowserFingerprintEnabled() bool { return s.c.BrowserFingerprintEnabled }
func (s staticConfig) BrowserFingerprintMode() string  { return s.c.BrowserFingerprintMode }
func (s staticConfig) BrowserFingerprintPreset() string {
	return s.c.BrowserFingerprintPreset
}
func (s staticConfig) BrowserFingerprintCustom() string { return s.c.BrowserFingerprintCustom }
func (s staticConfig) BrowserFingerprintTLSMode() string {
	return s.c.BrowserFingerprintTLSMode
}
func (s staticConfig) BrowserFingerprintTLSProfile() string {
	return s.c.BrowserFingerprintTLSProfile
}
func (s staticConfig) BrowserFingerprintTLSMin() int    { return s.c.BrowserFingerprintTLSMin }
func (s staticConfig) BrowserFingerprintTLSMax() int    { return s.c.BrowserFingerprintTLSMax }
func (s staticConfig) BrowserFingerprintLocale() string { return s.c.BrowserFingerprintLocale }
func (s staticConfig) BrowserFingerprintTLSPool() []string {
	out := make([]string, len(s.c.BrowserFingerprintTLSPool))
	copy(out, s.c.BrowserFingerprintTLSPool)
	return out
}
