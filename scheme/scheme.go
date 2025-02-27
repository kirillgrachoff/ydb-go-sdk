package scheme

import (
	"context"

	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Scheme"
)

type Client interface {
	DescribePath(ctx context.Context, path string) (e Entry, err error)
	MakeDirectory(ctx context.Context, path string) (err error)
	ListDirectory(ctx context.Context, path string) (d Directory, err error)
	RemoveDirectory(ctx context.Context, path string) (err error)
	ModifyPermissions(ctx context.Context, path string, opts ...PermissionsOption) (err error)
}

type EntryType uint

type Directory struct {
	Entry
	Children []Entry
}

const (
	EntryTypeUnknown EntryType = iota
	EntryDirectory
	EntryTable
	EntryPersQueueGroup
	EntryDatabase
	EntryRtmrVolume
	EntryBlockStoreVolume
	EntryCoordinationNode
	EntryTopic
)

func (t EntryType) String() string {
	switch t {
	default:
		return "Unknown"
	case EntryDirectory:
		return "Directory"
	case EntryTable:
		return "Table"
	case EntryPersQueueGroup:
		return "PersQueueGroup"
	case EntryDatabase:
		return "Name"
	case EntryRtmrVolume:
		return "RtmrVolume"
	case EntryBlockStoreVolume:
		return "BlockStoreVolume"
	case EntryCoordinationNode:
		return "CoordinationNode"
	case EntryTopic:
		return "Topic"
	}
}

type Entry struct {
	Name                 string
	Owner                string
	Type                 EntryType
	Permissions          []Permissions
	EffectivePermissions []Permissions
}

func (e *Entry) IsDirectory() bool {
	return e.Type == EntryDirectory
}

func (e *Entry) IsTable() bool {
	return e.Type == EntryTable
}

func (e *Entry) IsPersQueueGroup() bool {
	return e.Type == EntryPersQueueGroup
}

func (e *Entry) IsDatabase() bool {
	return e.Type == EntryDatabase
}

func (e *Entry) IsRtmrVolume() bool {
	return e.Type == EntryRtmrVolume
}

func (e *Entry) IsBlockStoreVolume() bool {
	return e.Type == EntryBlockStoreVolume
}

func (e *Entry) IsCoordinationNode() bool {
	return e.Type == EntryCoordinationNode
}

func (e *Entry) IsTopic() bool {
	return e.Type == EntryTopic
}

func (e *Entry) From(y *Ydb_Scheme.Entry) {
	*e = Entry{
		Name:                 y.Name,
		Owner:                y.Owner,
		Type:                 entryType(y.Type),
		Permissions:          makePermissions(y.Permissions),
		EffectivePermissions: makePermissions(y.EffectivePermissions),
	}
}

func entryType(t Ydb_Scheme.Entry_Type) EntryType {
	switch t {
	case Ydb_Scheme.Entry_DIRECTORY:
		return EntryDirectory
	case Ydb_Scheme.Entry_TABLE:
		return EntryTable
	case Ydb_Scheme.Entry_PERS_QUEUE_GROUP:
		return EntryPersQueueGroup
	case Ydb_Scheme.Entry_DATABASE:
		return EntryDatabase
	case Ydb_Scheme.Entry_RTMR_VOLUME:
		return EntryRtmrVolume
	case Ydb_Scheme.Entry_BLOCK_STORE_VOLUME:
		return EntryBlockStoreVolume
	case Ydb_Scheme.Entry_COORDINATION_NODE:
		return EntryCoordinationNode
	case Ydb_Scheme.Entry_TOPIC:
		return EntryTopic
	default:
		return EntryTypeUnknown
	}
}

func makePermissions(src []*Ydb_Scheme.Permissions) (dst []Permissions) {
	for _, p := range src {
		dst = append(dst, from(p))
	}
	return dst
}

func from(y *Ydb_Scheme.Permissions) (p Permissions) {
	return Permissions{
		Subject:         y.Subject,
		PermissionNames: y.PermissionNames,
	}
}

type Permissions struct {
	Subject         string
	PermissionNames []string
}

func (p Permissions) To(y *Ydb_Scheme.Permissions) {
	y.Subject = p.Subject
	y.PermissionNames = p.PermissionNames
}

func InnerConvertEntry(y *Ydb_Scheme.Entry) *Entry {
	res := &Entry{}
	res.From(y)
	return res
}
