// Package lfs implements Git LFS for the server: pointers and their
// scanning, the batch and locks API types, storing verified objects, and a
// client that fetches the objects of mirrors.
package lfs

// TransferBasic is the name of the Git LFS basic transfer protocol.
const TransferBasic = "basic"

// Limits on batch requests, over HTTP and SSH. A batch costs a storage stat
// and a database lookup per object, so the number of objects is capped.
// git-lfs sends 100 objects per batch by default (lfs.transfer.batchSize);
// the size limit leaves room for 1000 objects with long ref names. Over SSH
// the size limit applies to every command, apart from object data.
const (
	MaxBatchObjects     = 1000
	MaxBatchRequestSize = 1 << 20 // 1 MiB
)
