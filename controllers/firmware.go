package controllers

import (
	"fmt"
	"hash/crc32"
	"pannetrat.com/nocand/models"
	"pannetrat.com/nocand/models/nocan"
	"pannetrat.com/nocand/socket"
)

const (
	NODE_OP_NONE = iota
	NODE_OP_UPLOAD_FLASH
	NODE_OP_DOWNLOAD_FLASH
)

type NodeFirmwareOperation struct {
	Client    *socket.Client
	Operation int // NODE_OP_...
	Progress  *socket.NodeFirmwareProgress
	Firmware  *socket.NodeFirmware
}

func NewNodeFirmwareOperation(client *socket.Client, op int, progress *socket.NodeFirmwareProgress, firmware *socket.NodeFirmware) *NodeFirmwareOperation {
	return &NodeFirmwareOperation{Client: client, Operation: op, Progress: progress, Firmware: firmware}
}

const (
	FLASH_ORIGIN     uint32 = 0
	FLASH_LENGTH     uint32 = 0x40000
	FLASH_APP_ORIGIN uint32 = FLASH_ORIGIN + 0x2000 // 8K bootloader
	FLASH_APP_LENGTH uint32 = FLASH_LENGTH - 0x2000
	FLASH_PAGE_SIZE  uint32 = 64
)

func uint32ToBytes(u uint32, d []byte) []byte {
	d[0] = byte(u >> 24)
	d[1] = byte(u >> 16)
	d[2] = byte(u >> 8)
	d[3] = byte(u)
	return d[:4]
}

func uploadFirmware(node *models.Node, op *NodeFirmwareOperation) error {
	var address uint32
	var crc uint32
	var data [8]byte

	uint32ToBytes(FLASH_APP_ORIGIN, data[:])
	Bus.SendSystemMessage(node.Id, nocan.SYS_BOOTLOADER_SET_ADDRESS, 'F', data[:4])
	if _, err := Bus.ExpectSystemMessage(node.Id, nocan.SYS_BOOTLOADER_SET_ADDRESS_ACK); err != nil {
		op.Client.Put(socket.NodeFirmwareProgressEvent, op.Progress.Failed())
		return fmt.Errorf("SYS_BOOTLOADER_SET_ADDRESS failed for node %s, prior to erase operation, %s", node, err)
	}
	Bus.SendSystemMessage(node.Id, nocan.SYS_BOOTLOADER_ERASE, 0, nil)
	if _, err := Bus.ExpectSystemMessage(node.Id, nocan.SYS_BOOTLOADER_ERASE_ACK); err != nil {
		op.Client.Put(socket.NodeFirmwareProgressEvent, op.Progress.Failed())
		return fmt.Errorf("SYS_BOOTLOADER_ERASE failed for node %s, %s", node, err)
	}
	// TODO: check return code in ACK
	for _, block := range op.Firmware.Code {
		blocksize := uint32(len(block.Data))

		for page_offset := uint32(0); page_offset < blocksize; page_offset += FLASH_PAGE_SIZE {
			base_address := block.Offset + page_offset
			uint32ToBytes(base_address, data[:])
			Bus.SendSystemMessage(node.Id, nocan.SYS_BOOTLOADER_SET_ADDRESS, 'F', data[:4])
			if _, err := Bus.ExpectSystemMessage(node.Id, nocan.SYS_BOOTLOADER_SET_ADDRESS_ACK); err != nil {
				op.Client.Put(socket.NodeFirmwareProgressEvent, op.Progress.Failed())
				return fmt.Errorf("SYS_BOOTLOADER_SET_ADDRESS failed for node %s at address=0x%x, %s", node, address, err)
			}

			crc = 0
			for page_pos := uint32(0); page_pos < FLASH_PAGE_SIZE && page_offset+page_pos < blocksize; page_pos += 8 {
				rlen := copy(data[:], block.Data[page_offset+page_pos:])
				crc = crc32.Update(crc, crc32.IEEETable, data[:rlen])
				Bus.SendSystemMessage(node.Id, nocan.SYS_BOOTLOADER_WRITE, 0, data[:rlen])
				if _, err := Bus.ExpectSystemMessage(node.Id, nocan.SYS_BOOTLOADER_WRITE_ACK); err != nil {
					op.Client.Put(socket.NodeFirmwareProgressEvent, op.Progress.Failed())
					return fmt.Errorf("SYS_BOOTLOADER_WRITE failed for node %d at address=0x%x, %s", node, address, err)
				}
			}
			uint32ToBytes(crc, data[:])
			Bus.SendSystemMessage(node.Id, nocan.SYS_BOOTLOADER_WRITE, 1, data[:4])
			if _, err := Bus.ExpectSystemMessage(node.Id, nocan.SYS_BOOTLOADER_WRITE_ACK); err != nil {
				op.Client.Put(socket.NodeFirmwareProgressEvent, op.Progress.Failed())
				return fmt.Errorf("Final SYS_BOOTLOADER_WRITE failed for node %d at address=0x%x, %s", node, address, err)
			}
			// TODO: check return code in ACK
			if err := op.Client.Put(socket.NodeFirmwareProgressEvent, op.Progress.Update(socket.ProgressReport((page_offset*100)/blocksize), 0)); err != nil {
				return err
			}
		}
	}
	return op.Client.Put(socket.NodeFirmwareProgressEvent, op.Progress.Success())
}

func downloadFirmware(node *models.Node, op *NodeFirmwareOperation) error {
	var address, memlength uint32
	var i uint32
	var data [8]byte

	if op.Firmware.Limit > FLASH_APP_LENGTH {
		memlength = FLASH_APP_LENGTH
	} else {
		memlength = op.Firmware.Limit
	}
	block := make([]byte, 0, memlength)

	for i = 0; i < memlength/FLASH_PAGE_SIZE; i++ {
		address = FLASH_APP_ORIGIN + i*FLASH_PAGE_SIZE
		uint32ToBytes(address, data[:])
		Bus.SendSystemMessage(node.Id, nocan.SYS_BOOTLOADER_SET_ADDRESS, 'F', data[:4])
		if _, err := Bus.ExpectSystemMessage(node.Id, nocan.SYS_BOOTLOADER_SET_ADDRESS_ACK); err != nil {
			op.Client.Put(socket.NodeFirmwareProgressEvent, op.Progress.Failed())
			return fmt.Errorf("NOCAN_SYS_BOOTLOADER_SET_ADDRESS failed for node %d at address=0x%x, %s", node.Id, address, err)
		}

		for pos := uint32(0); pos < FLASH_PAGE_SIZE; pos += 8 {
			Bus.SendSystemMessage(node.Id, nocan.SYS_BOOTLOADER_READ, 8, nil)
			response, err := Bus.ExpectSystemMessage(node.Id, nocan.SYS_BOOTLOADER_READ_ACK)
			if err != nil {
				op.Client.Put(socket.NodeFirmwareProgressEvent, op.Progress.Failed())
				return fmt.Errorf("NOCAN_SYS_BOOTLOADER_READ failed for node %d at address=0x%x, %s", node, address, err)
			}

			block = append(block, response.Bytes()...)
			address += 8
		}
		if err := op.Client.Put(socket.NodeFirmwareProgressEvent, op.Progress.Update(socket.ProgressReport((address-FLASH_APP_ORIGIN)*100/memlength), address-FLASH_APP_ORIGIN)); err != nil {
			return err
		}
	}
	op.Client.Put(socket.NodeFirmwareProgressEvent, op.Progress.Success())

	op.Firmware.AppendBlock(FLASH_APP_ORIGIN, block)

	return op.Client.Put(socket.NodeFirmwareDownloadEvent, op.Firmware)
}
