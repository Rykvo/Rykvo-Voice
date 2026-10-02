package main

import (
	"context"
	"errors"
	"log"

	"github.com/emiago/sipgo/sip"
)

func sipByeConfirmed(record string, response *sip.Response, err error) bool {
	status, result := 0, "no-response"
	if response != nil {
		status = response.StatusCode
		result = "rejected"
	}
	confirmed := err == nil && (status == 200 || status == 481)
	switch {
	case confirmed:
		result = "confirmed"
	case errors.Is(err, context.DeadlineExceeded):
		result = "timeout"
	case err != nil:
		result = "transport-error"
	}
	log.Printf("SIP call %s BYE: result=%s status=%d", record, result, status)
	return confirmed
}
