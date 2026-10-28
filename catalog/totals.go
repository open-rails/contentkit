package catalog

// TotalsQuery selects the kinds and languages to total.
type TotalsQuery struct {
	// Kinds are content kinds; empty means all of them.
	Kinds []string
	// Languages are the languages Total.Languages reports; empty means every
	// language with a counted item. Kind totals ignore it.
	Languages []string
	// Domain is the stable domain name whose scope applies; empty selects
	// main. The HTTP route takes it from the request's host.
	Domain string
}

// Total sizes one kind. Public counts what an anonymous visitor's listing
// shows and opens; Tiers[t] every eligible item a holder of t opens,
// members-listed ones included. An item counts once in the kind's total and
// once per language any of its eligible versions serves, never per version.
// The site's scope and the default content filters apply.
type Total struct {
	Public    int64                    `json:"public"`
	Tiers     map[string]int64         `json:"tiers"`
	Languages map[string]LanguageTotal `json:"languages"`
}

// LanguageTotal sizes one kind in one language, as Total does.
type LanguageTotal struct {
	Public int64            `json:"public"`
	Tiers  map[string]int64 `json:"tiers"`
}

// Totals is the catalog's size by content kind.
type Totals struct {
	Kinds map[string]Total `json:"kinds"`
}
