package value

import (
	"bytes"

	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb"

	"github.com/ydb-platform/ydb-go-sdk/v3/internal/value/exp/allocator"
)

type dyNumberValue struct {
	v string
}

func (v dyNumberValue) toString(buffer *bytes.Buffer) {
	a := allocator.New()
	defer a.Free()
	v.getType().toString(buffer)
	valueToString(buffer, v.getType(), v.toYDBValue(a))
}

func (v dyNumberValue) String() string {
	var buf bytes.Buffer
	v.toString(&buf)
	return buf.String()
}

func (dyNumberValue) getType() T {
	return TypeDyNumber
}

func (dyNumberValue) toYDBType(*allocator.Allocator) *Ydb.Type {
	return primitive[TypeDyNumber]
}

func (v *dyNumberValue) toYDBValue(a *allocator.Allocator) *Ydb.Value {
	vv := a.Text()
	if v != nil {
		vv.TextValue = v.v
	}

	vvv := a.Value()
	vvv.Value = vv

	return vvv
}

func DyNumberValue(v string) *dyNumberValue {
	return &dyNumberValue{v: v}
}
