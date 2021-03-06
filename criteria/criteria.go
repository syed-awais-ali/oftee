// Package criteria is used to manage match criteria on a network packet. It is
// expected that the match criteria will expand as required over time. The
// intent is that the critieria follows (closely matches) that used to specify
// match criteria in the `ovs-ofctl` command.
package criteria

// Defines the bit patterns used to indicate which values are set in the
// match criteria structure.
const (
	BitEmpty  = 0x0
	BitDLType = 1 << 0
)

// Criteria is used to maintain match criteria values along with a bit set to
// indicate which values are set.
type Criteria struct {
	Set    uint64
	DlType uint16
}

// Match compares match criteria against a given criteria to determine if there
// is a match and returns `true` if they match, else `false`. A match is defined
// as when all the values set in the target criteria are included in the the
// state criteria and their values are equal. The state criteria may have
// additional values that are not in the target criteria and the values will
// still be considered matched.
func (c *Criteria) Match(state Criteria) bool {
	if c.Set&BitDLType > 0 && (state.Set&BitDLType == 0 || c.DlType != state.DlType) {
		return false
	}
	return true
}
