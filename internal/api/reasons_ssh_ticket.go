// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// SSH over an attach ticket (sshgateway_ticket.go, attach_ticket_purpose.go): the ticket mint's purposes and the
// GET /runs/{id}/ssh door. Sentences are capitalised inline literals at their writeErrorReason calls.
const (
	reasonAttachTicketPurposeInvalid = "attach_ticket_purpose_invalid" // 400: purpose names no door (ssh-exec, ssh-forward, ssh-editor, attach-forward)
	reasonAttachTicketPortsInvalid   = "attach_ticket_ports_invalid"   // 400: ports are not 1 to 16 distinct ports from 1 to 65535, or ride a purpose that takes none
	reasonSSHTicketUnsupportedCaller = "ssh_ticket_unsupported_caller" // 403: an SSH ticket is for a person signed in directly
	reasonSSHGatewayOff              = "ssh_gateway_off"               // 409: the SSH gateway is off (WARDYN_SSH_LISTEN)
	reasonSSHTicketsOff              = "ssh_tickets_off"               // 409: WARDYN_SSH_TICKETS is off
	reasonSSHTicketChannelOff        = "ssh_ticket_channel_off"        // 409: a kind the purpose needs is left out of WARDYN_SSH_CHANNELS
	reasonSSHGatewayUnavailable      = "ssh_gateway_unavailable"       // 503: the gateway's serve loop is not up on this replica
	reasonSSHGatewayBusy             = "ssh_gateway_busy"              // 503: the handshake bounds are full
	reasonSSHTicketUnverifiable      = "ssh_ticket_unverifiable"       // 503: the run or the governance profile could not be read
)
