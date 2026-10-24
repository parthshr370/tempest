package compaction

// ModelSettings reserves output room and keeps the recent tail inside the window.
func ModelSettings(window, output int) CompactionSettings {
	settings := DefaultCompactionSettings
	const minimumSummaryShare = 3
	const maximumOutputShare = 2
	settings.ReserveTokens = min(max(settings.ReserveTokens, output), max(1, window/maximumOutputShare))
	settings.KeepRecentTokens = min(settings.KeepRecentTokens, max(1, (window-settings.ReserveTokens)/minimumSummaryShare))
	return settings
}

// Thresholds returns the speculative start and blocking input budgets.
func Thresholds(window int, settings CompactionSettings) (soft, hard int) {
	hard = max(1, window-settings.ReserveTokens)
	soft = settings.SoftThresholdTokens
	if soft == 0 {
		soft = max(1, hard-settings.ReserveTokens/2)
	}
	return min(soft, hard), hard
}
