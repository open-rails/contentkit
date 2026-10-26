package entities

// MatchesIdentity compares caller-supplied names and localized text using the
// same normalization as writes. It is useful when replaying an acknowledged
// creation: omitted alias kinds, language casing and surrounding name/sort-key
// whitespace must not turn an identical request into a conflicting identity.
func (d EntityDetail) MatchesIdentity(names []Name, text []Text) bool {
	givenNames, err := validateNames(d.ID, names)
	if err != nil || !sameNames(givenNames, d.Names) {
		return false
	}
	givenText, err := validateText(d.ID, text)
	if err != nil || len(givenText) != len(d.Text) {
		return false
	}
	stored := make(map[string]Text, len(d.Text))
	for _, value := range d.Text {
		stored[value.Language] = value
	}
	for _, value := range givenText {
		if current, ok := stored[value.Language]; !ok || current.Description != value.Description || current.SortKey != value.SortKey {
			return false
		}
	}
	return true
}
