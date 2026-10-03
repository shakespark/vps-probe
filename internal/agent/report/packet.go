// Package report turns Reports into encrypted UDP packets and delivers them
// with acknowledgement and retry.
package report

import (
	"crypto/cipher"
	"fmt"
	"math/rand/v2"

	"google.golang.org/protobuf/proto"

	pb "github.com/shakespark/vps-probe/internal/proto/probev1"
	"github.com/shakespark/vps-probe/internal/wire"
)

// Packet is one sealed datagram and the report id it carries.
type Packet struct {
	ID    uint64
	TS    int64
	Bytes []byte
}

// maxPlain is the largest protobuf payload that fits one packet for node.
func maxPlain(node string) int {
	return wire.MaxPacket - wire.Overhead - len(node)
}

// Packetize seals rep into one or more packets. When it doesn't fit in one,
// its fields are packed greedily into several Reports sharing rep.Ts, each
// with its own id. Any split is valid: the server merges pieces field by
// field. Ids are assigned here, overwriting rep.Id.
func Packetize(aead cipher.AEAD, node string, rep *pb.Report) ([]Packet, error) {
	limit := maxPlain(node)
	rep.Id = rand.Uint64()
	if proto.Size(rep) <= limit {
		p, err := seal(aead, node, rep)
		if err != nil {
			return nil, err
		}
		return []Packet{p}, nil
	}

	// Each item adds one field (or one repeated element) to a piece.
	var items []func(*pb.Report)
	// Small and wanted first: what the overview shows.
	if rep.Cpu != nil {
		items = append(items, func(r *pb.Report) { r.Cpu = rep.Cpu })
	}
	if rep.Load != nil {
		items = append(items, func(r *pb.Report) { r.Load = rep.Load })
	}
	if rep.Mem != nil {
		items = append(items, func(r *pb.Report) { r.Mem = rep.Mem })
	}
	if rep.Sockets != nil {
		items = append(items, func(r *pb.Report) { r.Sockets = rep.Sockets })
	}
	for _, t := range rep.Traffic {
		items = append(items, func(r *pb.Report) { r.Traffic = append(r.Traffic, t) })
	}
	for _, n := range rep.Net {
		items = append(items, func(r *pb.Report) { r.Net = append(r.Net, n) })
	}
	if rep.Sys != nil {
		items = append(items, func(r *pb.Report) { r.Sys = rep.Sys })
	}
	for _, d := range rep.Disks {
		items = append(items, func(r *pb.Report) { r.Disks = append(r.Disks, d) })
	}
	for _, p := range rep.Pings {
		items = append(items, func(r *pb.Report) { r.Pings = append(r.Pings, p) })
	}

	newPiece := func() *pb.Report { return &pb.Report{Ts: rep.Ts, Id: rand.Uint64()} }
	var pieces []*pb.Report
	cur := newPiece()
	curEmpty := true
	for _, add := range items {
		trial := proto.Clone(cur).(*pb.Report)
		add(trial)
		if proto.Size(trial) <= limit {
			cur, curEmpty = trial, false
			continue
		}
		if curEmpty {
			return nil, fmt.Errorf("report: a single field exceeds the %d byte packet budget", limit)
		}
		pieces = append(pieces, cur)
		cur = newPiece()
		add(cur)
		curEmpty = false
		if proto.Size(cur) > limit {
			return nil, fmt.Errorf("report: a single field exceeds the %d byte packet budget", limit)
		}
	}
	if !curEmpty || len(pieces) == 0 {
		pieces = append(pieces, cur)
	}

	out := make([]Packet, 0, len(pieces))
	for _, pc := range pieces {
		p, err := seal(aead, node, pc)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

func seal(aead cipher.AEAD, node string, r *pb.Report) (Packet, error) {
	b, err := proto.Marshal(r)
	if err != nil {
		return Packet{}, err
	}
	pkt, err := wire.Seal(aead, wire.TypeReport, node, b)
	if err != nil {
		return Packet{}, err
	}
	return Packet{ID: r.Id, TS: r.Ts, Bytes: pkt}, nil
}
