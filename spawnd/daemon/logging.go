package daemon

import (
	"context"
	"time"

	"github.com/SoftwareDefinedBuildings/spawnpoint/service"
	bw2 "github.com/immesys/bw2bind"
	"github.com/pkg/errors"
)

func (daemon *SpawnpointDaemon) tailLogs(ctx context.Context, svc *runningService, fromBeginning bool) {
	logChan, errChan := daemon.backend.TailService(ctx, svc.ID, fromBeginning)
	bw2Iface := daemon.bw2Service.RegisterInterface(svc.Name, "i.spawnable")
	alive := true
	pending := time.AfterFunc(1*time.Minute, func() {
		select {
		case <-ctx.Done():
			return
		default:
		}
		daemon.logger.Debugf("(%s) Logging timeout has expired", svc.Name)
		alive = false
	})

	keepAliveHandle, err := bw2Iface.SubscribeSlotH("keepLogAlive", func(msg *bw2.SimpleMessage) {
		daemon.logger.Debugf("(%s) Received log keep-alive message", svc.Name)
		select {
		case <-ctx.Done():
			daemon.logger.Debugf("(%s) No longer tailing log, message likely generated by unsubscribe", svc.Name)
			return
		default:
		}
		pending.Stop()
		pending.Reset(1 * time.Minute)
		alive = true
	})
	if err != nil {
		daemon.logger.Errorf("(%s) Failed to subscribe to log keep-alive slot: %s", svc.Name, err)
	}

	defer func() {
		daemon.logger.Debugf("(%s) Stopping log tailing", svc.Name)
		pending.Stop()
		if err := daemon.bw2Client.Unsubscribe(keepAliveHandle); err != nil {
			daemon.logger.Errorf("(%s) Failed to unsubscribe from log keep-alive slot", svc.Name)
		} else {
			daemon.logger.Debugf("(%s) Unsubscribed from log keep-alive slot", svc.Name)
		}
	}()

	for {
		select {
		case message := <-logChan:
			if len(message) == 0 {
				daemon.logger.Debugf("(%s) Received empty log message", svc.Name)
				// This only happens when the channel has been closed
				// Which happens when container stops running, or when an error has occurred
				select {
				case err := <-errChan:
					daemon.logger.Errorf("(%s) Error occurred while tailing logs: %s", svc.Name, err)
					return
				default:
					return
				}
			}

			if alive {
				daemon.logger.Debugf("(%s) New log entry available, sending", svc.Name)
				po, err := bw2.CreateMsgPackPayloadObject(bw2.PONumSpawnpointLog, service.LogMessage{
					Contents:  message,
					Timestamp: time.Now().UnixNano(),
				})
				if err != nil {
					daemon.logger.Errorf("(%s) Failed to serialize log message: %s", svc.Name, err)
				}

				if err = bw2Iface.PublishSignal("log", po); err != nil {
					daemon.logger.Errorf("(%s) Failed to publish log message: %s", svc.Name, err)
				}
			} else {
				daemon.logger.Debugf("(%s) New log entry available, but no active recipients", svc.Name)
			}
			break

		case <-ctx.Done():
			return
		}
	}
}

func (daemon *SpawnpointDaemon) publishLogMessage(svcName string, msg string) error {
	iFace := daemon.bw2Service.RegisterInterface(svcName, "i.spawnable")
	logMessage := service.LogMessage{
		Timestamp: time.Now().UnixNano(),
		Contents:  msg,
	}
	logMessagePo, err := bw2.CreateMsgPackPayloadObject(bw2.PONumSpawnpointLog, logMessage)
	if err != nil {
		return errors.Wrap(err, "Failed to create msgpack object")
	}

	if err = iFace.PublishSignal("log", logMessagePo); err != nil {
		return errors.Wrap(err, "Bosswave publication failed")
	}
	return nil
}
