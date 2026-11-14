// Package diskspace exposes local filesystem availability for optional log writes.
package diskspace

// LogReserveBytes keeps a modest emergency margin for primary data and cleanup.
// This is not a global disk quota: other processes can consume the same space.
const LogReserveBytes int64 = 256 << 20
