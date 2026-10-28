package signalstore

func init() {
	base := []columnSpec{{"tenant", "LowCardinality(String)"}, {"content_kind", "LowCardinality(String)"},
		{"domain", "LowCardinality(String)"}, {"language", "LowCardinality(String)"}, {"day", "Date"}}
	daily := tableSpec{engine: "MergeTree", partitionKey: "(tenant, content_kind, domain, language, day)",
		sortingKey: "tenant, content_kind, domain, language, day, content_id",
		columns:    append(append([]columnSpec{}, base...), columnSpec{"content_id", "UUID"}, columnSpec{"readers", "UInt64"}, columnSpec{"eligible", "UInt64"})}
	expectedSchema["popularity_daily"] = daily
	expectedSchema["popularity_daily_staging"] = daily
	expectedSchema["popularity_daily_prepared"] = tableSpec{engine: "ReplacingMergeTree", version: "computed_at",
		partitionKey: "toYYYYMM(day)", sortingKey: "tenant, content_kind, domain, language, day",
		columns: append(append([]columnSpec{}, base...), columnSpec{"computed_at", "DateTime64(6, 'UTC')"})}
}
