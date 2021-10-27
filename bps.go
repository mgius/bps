// Go library for handling BPS patch files, as commonly used in romfile patching
package bps

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
)

var (
	bps_header = []byte("BPS1")
)

const (
	sourceRead = iota
	targetRead
	sourceCopy
	targetCopy
)

type BPSPatch struct {
	SourceSize     uint64
	TargetSize     uint64
	MetadataSize   uint64
	Metadata       string
	Actions        []byte
	SourceChecksum uint32
	TargetChecksum uint32
	PatchChecksum  uint32
}

// Apply a BPS patch file to the specified source file.  The checksum of the
// source file and the returned bytes will be verified and an error returned if
// either fails
func (patch *BPSPatch) PatchSourceFile(sourcefile *os.File) (target_data []byte, err error) {
	// Read and validate source file
	source_data := make([]byte, patch.SourceSize)

	_, err = sourcefile.Read(source_data)
	if err != nil {
		err = fmt.Errorf("Sourcefile Read: %w", err)
		return
	}

	calculated_source_checksum := crc32.ChecksumIEEE(source_data)
	if calculated_source_checksum != patch.SourceChecksum {
		err = errors.New("Source File checksum mismatch")
		return
	}

	// Initialize target data byte slice
	target_data = make([]byte, patch.TargetSize)

	remaining_actions := patch.Actions

	var (
		output_offset uint64
		source_offset uint64
		target_offset uint64
	)

	for len(remaining_actions) > 0 {
		var header uint64
		header, remaining_actions, err = bps_read_num(remaining_actions)
		if err != nil {
			err = fmt.Errorf("Read Action: %w", err)
			return
		}
		// First two bits of the header are the action num
		action_num := header & 0b11
		// Remaining bits are the length minus one
		length := (header >> 2) + 1

		switch action_num {
		case sourceRead:
			// Copy length bytes from source file to target file, using the output offset as the index for both source and target
			copy(target_data[output_offset:output_offset+length], source_data[output_offset:output_offset+length])
			output_offset += length
		case targetRead:
			// copy length bytes from patch file to target file
			copy(target_data[output_offset:output_offset+length], remaining_actions[:length])
			output_offset += length
			remaining_actions = remaining_actions[length:]
		case sourceCopy:
			// copy length bytes from somewhere else in the source file.  Increment or decrement the source offset before copying
			var (
				data uint64
			)
			data, remaining_actions, err = bps_read_num(remaining_actions)
			if err != nil {
				err = fmt.Errorf("Source copy data read: %w", err)
				return
			}
			if data&1 == 1 {
				source_offset -= data >> 1
			} else {
				source_offset += data >> 1
			}
			copy(target_data[output_offset:output_offset+length], source_data[source_offset:source_offset+length])
			source_offset += length
			output_offset += length
		case targetCopy:
			// copy data from somewhere else in the target file.  Increment or decrement the target offset before copying
			var (
				data uint64
			)
			data, remaining_actions, err = bps_read_num(remaining_actions)
			if err != nil {
				err = fmt.Errorf("Target Copy Read %w", err)
				return
			}
			if data&1 == 1 {
				target_offset -= data >> 1
			} else {
				target_offset += data >> 1
			}
			// sadly, cannot use copy for this, because we might be copying from areas we haven't written yet
			for length > 0 {
				target_data[output_offset] = target_data[target_offset]
				output_offset += 1
				target_offset += 1
				length--
			}
		}

	}

	calculated_target_checksum := crc32.ChecksumIEEE(target_data)
	if calculated_target_checksum != patch.TargetChecksum {
		// This is likely a bug in the implementation, if we hit it
		err = errors.New("Target Checksum mismatch.")
	}

	return

}

// Read a BPS patch file, verifying the patch checksum
func FromFile(patchfile *os.File) (patch BPSPatch, err error) {
	filestat, err := patchfile.Stat()
	if err != nil {
		err = fmt.Errorf("Error performing stat on patchfile: %w", err)
	}
	filesize := filestat.Size()

	full_file := make([]byte, filesize)
	patchfile.Read(full_file)

	if !bytes.Equal(full_file[:len(bps_header)], bps_header) {
		return BPSPatch{}, errors.New("Magic Header Incorrect")
	}

	remaining := full_file[len(bps_header):]

	// TODO: error handling
	source_size, remaining, err := bps_read_num(remaining)
	if err != nil {
		err = fmt.Errorf("Error reading source size: %w", err)
		return
	}

	target_size, remaining, err := bps_read_num(remaining)
	if err != nil {
		err = fmt.Errorf("Error reading target size: %w", err)
	}

	metadata_size, remaining, err := bps_read_num(remaining)
	if err != nil {
		err = fmt.Errorf("Error reading metadata size: %w", err)
	}

	metadata, remaining := string(remaining[:metadata_size]), remaining[metadata_size:]

	action_len := len(remaining) - 12
	actions, remaining := remaining[:action_len], remaining[action_len:]

	source_checksum := binary.LittleEndian.Uint32(remaining[:4])
	target_checksum := binary.LittleEndian.Uint32(remaining[4:8])
	patch_checksum := binary.LittleEndian.Uint32(remaining[8:12])

	// TODO: validate patch_checksum
	// patch checksum is run over the whole file minus the patch checksum
	calculated_patch_checksum := crc32.ChecksumIEEE(full_file[:len(full_file)-4])
	if calculated_patch_checksum != patch_checksum {
		return BPSPatch{}, errors.New("Patch checksum did not verify")
	}

	return BPSPatch{
		SourceSize:     source_size,
		TargetSize:     target_size,
		MetadataSize:   metadata_size,
		Metadata:       metadata,
		Actions:        actions,
		SourceChecksum: source_checksum,
		TargetChecksum: target_checksum,
		PatchChecksum:  patch_checksum,
	}, nil

}

// Serialize a uint64 into a BPS variable length encoded byte stream Should
// probably switch to return bytes at some point?  Mostly this is used for test
// cases ATM
func bps_write_num(bytewriter io.ByteWriter, num uint64) error {
	for true {
		// slice off the lowest 7 bits of num
		x := byte(num & 0x7f)
		// shift the lowest 7 bits out of the num
		num >>= 7

		// If we've encoded all bits of the number into either x or the byte
		// stream, write out x with the end of number bit set
		if num == 0 {
			err := bytewriter.WriteByte(0x80 | x)
			if err != nil {
				return err
			}
			break
		}

		// Otherwise, write out the byte and loop around
		bytewriter.WriteByte(x)

		// weird optimization for "one"?
		// I don't understand the purpose of this optimization, and the
		// reference decode implementation doesn't seem to handle this
		// optimization at all, and every other bps impl I've seen doesn't do
		// this either
		num--
	}

	return nil
}

// Read a BPS serialized variable length encoded integer from the provided byte slice.
func bps_read_num(stream []byte) (data uint64, remainder []byte, err error) {
	var (
		bytes_read int    = 0
		shift      uint64 = 1
	)

	for bytes_read < len(stream) {
		// Grab the next byte and indicate we read one.
		var x = stream[bytes_read]
		bytes_read++

		// Mask off the eigth bit.  Multiply the remaining 7 bits by the shift,
		// and add into our data parameter.
		data += uint64((x & 0x7f)) * shift

		// If the 8th bit is set, we've reached end of number
		if (x & 0x80) == 0x80 {
			remainder = stream[bytes_read:]
			return
		}
		// Increase the shift so that further reads represent higher bits in the read number
		shift <<= 7

		// I think this has to do with the way the encoding subtracts one from
		// the data as it goes, so you add "one" as we go?  But what about the first one?
		data += shift
	}

	err = errors.New("bps_read_num: Ran out of bytes before termination bit was set")

	return
}
