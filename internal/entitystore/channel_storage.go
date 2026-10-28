package entitystore

// ChannelStorage is the reserved persistence handle used by the channels
// module. It cannot read or mutate descriptive entities. Normal stores cannot
// read or mutate channels, even through raw IDs or management endpoints.
func (s *Store) ChannelStorage() *Store {
	out := *s
	out.channels = true
	return &out
}

func (s *Store) domainPredicate(alias string) string {
	if s.channels {
		return alias + ".kind = 'channel'"
	}
	return alias + ".kind <> 'channel'"
}
