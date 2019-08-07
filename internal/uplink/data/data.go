package data

import (
	"context"
	"fmt"
	"time"

	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"

	"github.com/brocaar/loraserver/api/as"
	"github.com/brocaar/loraserver/api/common"
	"github.com/brocaar/loraserver/api/geo"
	"github.com/brocaar/loraserver/api/gw"
	"github.com/brocaar/loraserver/api/nc"
	"github.com/brocaar/loraserver/internal/backend/applicationserver"
	"github.com/brocaar/loraserver/internal/backend/controller"
	"github.com/brocaar/loraserver/internal/backend/geolocationserver"
	"github.com/brocaar/loraserver/internal/band"
	"github.com/brocaar/loraserver/internal/config"
	datadown "github.com/brocaar/loraserver/internal/downlink/data"
	"github.com/brocaar/loraserver/internal/downlink/data/classb"
	"github.com/brocaar/loraserver/internal/framelog"
	"github.com/brocaar/loraserver/internal/helpers"
	"github.com/brocaar/loraserver/internal/maccommand"
	"github.com/brocaar/loraserver/internal/models"
	"github.com/brocaar/loraserver/internal/storage"
	"github.com/brocaar/lorawan"

	MQTT "github.com/eclipse/paho.mqtt.golang"
)

const applicationClientTimeout = time.Second

var tasks = []func(*dataContext) error{
	setContextFromDataPHYPayload,
	roamIfNeeded,
	getDeviceSessionForPHYPayload,
	decryptFOptsMACCommands,
	decryptFRMPayloadMACCommands,
	logUplinkFrame,
	getDeviceProfile,
	getServiceProfile,
	getApplicationServerClientForDataUp,
	resolveDeviceLocation,
	setADR,
	setUplinkDataRate,
	setBeaconLocked,
	sendRXInfoToNetworkController,
	handleFOptsMACCommands,
	handleFRMPayloadMACCommands,
	storeDeviceGatewayRXInfoSet,
	appendMetaDataToUplinkHistory,
	sendFRMPayloadToApplicationServer,
	setLastRXInfoSet,
	syncUplinkFCnt,
	saveDeviceSession,
	handleUplinkACK,
	handleDownlink,
}

var (
	getDownlinkDataDelay time.Duration
	disableMACCommands   bool
)

// Setup configures the package.
func Setup(conf config.Config) error {
	getDownlinkDataDelay = conf.NetworkServer.GetDownlinkDataDelay
	disableMACCommands = conf.NetworkServer.NetworkSettings.DisableMACCommands

	return nil
}

type dataContext struct {
	RXPacket                models.RXPacket
	MACPayload              *lorawan.MACPayload
	DeviceSession           storage.DeviceSession
	DeviceProfile           storage.DeviceProfile
	ServiceProfile          storage.ServiceProfile
	ApplicationServerClient as.ApplicationServerServiceClient
	MACCommandResponses     []storage.MACCommandBlock
	MustSendDownlink        bool
}

// Handle handles an uplink data frame
func Handle(rxPacket models.RXPacket) error {
	ctx := dataContext{
		RXPacket: rxPacket,
	}

	for _, t := range tasks {
		if err := t(&ctx); err != nil {
			return err
		}
	}

	return nil
}

func setContextFromDataPHYPayload(ctx *dataContext) error {
	macPL, ok := ctx.RXPacket.PHYPayload.MACPayload.(*lorawan.MACPayload)
	if !ok {
		return fmt.Errorf("expected *lorawan.MACPayload, got: %T", ctx.RXPacket.PHYPayload.MACPayload)
	}
	ctx.MACPayload = macPL
	return nil
}

func roamIfNeeded(ctx *dataContext) error {
	// 1. check if roaming is activated
	// 2. check if it's a non-null net-id (and if it's not ours)

	devAddr := ctx.RXPacket.PHYPayload.MACPayload.(*lorawan.MACPayload).FHDR.DevAddr
	nwkID := devAddr.NwkID()

	if  devAddr == [4]byte{0, 18, 79, 17} {  // if net id is not null and not ours ...
		url := findURLForNwkID(nwkID)
		sendRoamedPacket(url, ctx)
	}

	return nil
}

func sendRoamedPacket(url string, ctx *dataContext) {

	rx := ctx.RXPacket
	phyBinary, err  := rx.PHYPayload.MarshalBinary()
	u := gw.UplinkFrame{TxInfo:rx.TXInfo,
		RxInfo:rx.RXInfoSet[0],
		PhyPayload: phyBinary}

	// this is still a POC and it should be sent to mqtt (because not standard, because backend may not be mqtt, ...)
	// instead, I'ld like to add a new rest endpoint  in grpc or json (that could become a standard ^^) :
	//          It will then check roaming policies of the entering packet then send to his own gw backend (directly
	//          into his chan, so it does not reach mqtt.
	// Also, it would be better to have a special GW, only for roaming.
	opts := MQTT.NewClientOptions()
	opts.AddBroker(url)
	opts.SetClientID("xx")
	opts.SetUsername("")
	opts.SetPassword("")
	opts.SetCleanSession(false)

	client := MQTT.NewClient(opts)
	if token := client.Connect(); token.Wait() && token.Error() != nil {
		panic(token.Error())
	}

	messageBytes, err := u.XXX_Marshal([]byte{}, true)
	fmt.Println(err)
	token := client.Publish("gateway/b827ebfffef724e0/event/up", 0, false, messageBytes)
	token.Wait()


	client.Disconnect(250)
	fmt.Println(u)
}

func findURLForNwkID(nwkID []byte)  string {
	// first check local memory, then query dns 
	return  "tcp://192.168.1.17:1883"
}

func getDeviceSessionForPHYPayload(ctx *dataContext) error {
	txDR, err := helpers.GetDataRateIndex(true, ctx.RXPacket.TXInfo, band.Band())
	if err != nil {
		return errors.Wrap(err, "get data-rate index error")
	}

	var txCh int
	for _, defaultChannel := range []bool{true, false} {
		i, err := band.Band().GetUplinkChannelIndex(int(ctx.RXPacket.TXInfo.Frequency), defaultChannel)
		if err != nil {
			continue
		}

		c, err := band.Band().GetUplinkChannel(i)
		if err != nil {
			return errors.Wrap(err, "get channel error")
		}

		// there could be multiple channels using the same frequency, but with different data-rates.
		// eg EU868:
		//  channel 1 (868.3 DR 0-5)
		//  channel x (868.3 DR 6)
		if c.MinDR <= txDR && c.MaxDR >= txDR {
			txCh = i
		}
	}

	ds, err := storage.GetDeviceSessionForPHYPayload(storage.RedisPool(), ctx.RXPacket.PHYPayload, txDR, txCh)
	if err != nil {
		return errors.Wrap(err, "get device-session error")
	}
	ctx.DeviceSession = ds

	return nil
}

func logUplinkFrame(ctx *dataContext) error {
	uplinkFrameSet, err := framelog.CreateUplinkFrameSet(ctx.RXPacket)
	if err != nil {
		return errors.Wrap(err, "create uplink frame-log error")
	}

	if err := framelog.LogUplinkFrameForDevEUI(storage.RedisPool(), ctx.DeviceSession.DevEUI, uplinkFrameSet); err != nil {
		log.WithError(err).Error("log uplink frame for device error")
	}

	return nil
}

func getDeviceProfile(ctx *dataContext) error {
	dp, err := storage.GetAndCacheDeviceProfile(storage.DB(), storage.RedisPool(), ctx.DeviceSession.DeviceProfileID)
	if err != nil {
		return errors.Wrap(err, "get device-profile error")
	}
	ctx.DeviceProfile = dp

	return nil
}

func getServiceProfile(ctx *dataContext) error {
	sp, err := storage.GetAndCacheServiceProfile(storage.DB(), storage.RedisPool(), ctx.DeviceSession.ServiceProfileID)
	if err != nil {
		return errors.Wrap(err, "get service-profile error")
	}
	ctx.ServiceProfile = sp

	return nil
}

func setADR(ctx *dataContext) error {
	ctx.DeviceSession.ADR = ctx.MACPayload.FHDR.FCtrl.ADR
	return nil
}

func setUplinkDataRate(ctx *dataContext) error {
	currentDR, err := helpers.GetDataRateIndex(true, ctx.RXPacket.TXInfo, band.Band())
	if err != nil {
		return errors.Wrap(err, "get data-rate error")
	}

	// The node changed its data-rate. Possibly the node did also reset its
	// tx-power to max power. Because of this, we need to reset the tx-power
	// at the network-server side too.
	if ctx.DeviceSession.DR != currentDR {
		ctx.DeviceSession.TXPowerIndex = 0
	}
	ctx.DeviceSession.DR = currentDR

	return nil
}

// appendMetaDataToUplinkHistory appends uplink related meta-data to the
// uplink history in the device-session.
// As this also stores the TXPower, this function must be called after
// processing the mac-commands (we might have asked the device to change
// its TXPower and if one of the mac-commands contains a LinkADRReq ACK
// this will update the TXPowerIndex on the device-session).
func appendMetaDataToUplinkHistory(ctx *dataContext) error {
	var maxSNR float64
	for i, rxInfo := range ctx.RXPacket.RXInfoSet {
		// as the default value is 0 and the LoRaSNR can be negative, we always
		// set it when i == 0 (the first item from the slice)
		if i == 0 || rxInfo.LoraSnr > maxSNR {
			maxSNR = rxInfo.LoraSnr
		}
	}

	ctx.DeviceSession.AppendUplinkHistory(storage.UplinkHistory{
		FCnt:         ctx.MACPayload.FHDR.FCnt,
		GatewayCount: len(ctx.RXPacket.RXInfoSet),
		MaxSNR:       maxSNR,
		TXPowerIndex: ctx.DeviceSession.TXPowerIndex,
	})

	return nil
}

func storeDeviceGatewayRXInfoSet(ctx *dataContext) error {
	dr, err := helpers.GetDataRateIndex(true, ctx.RXPacket.TXInfo, band.Band())
	if err != nil {
		errors.Wrap(err, "get data-rate error")
	}

	rxInfoSet := storage.DeviceGatewayRXInfoSet{
		DevEUI: ctx.DeviceSession.DevEUI,
		DR:     dr,
	}

	for i := range ctx.RXPacket.RXInfoSet {
		rxInfoSet.Items = append(rxInfoSet.Items, storage.DeviceGatewayRXInfo{
			GatewayID: helpers.GetGatewayID(ctx.RXPacket.RXInfoSet[i]),
			RSSI:      int(ctx.RXPacket.RXInfoSet[i].Rssi),
			LoRaSNR:   ctx.RXPacket.RXInfoSet[i].LoraSnr,
		})
	}

	err = storage.SaveDeviceGatewayRXInfoSet(storage.RedisPool(), rxInfoSet)
	if err != nil {
		return errors.Wrap(err, "save device gateway rx-info set error")
	}

	return nil
}

func getApplicationServerClientForDataUp(ctx *dataContext) error {
	rp, err := storage.GetRoutingProfile(storage.DB(), ctx.DeviceSession.RoutingProfileID)
	if err != nil {
		return errors.Wrap(err, "get routing-profile error")
	}

	asClient, err := applicationserver.Pool().Get(rp.ASID, []byte(rp.CACert), []byte(rp.TLSCert), []byte(rp.TLSKey))
	if err != nil {
		return errors.Wrap(err, "get application-server client error")
	}

	ctx.ApplicationServerClient = asClient

	return nil
}

func resolveDeviceLocation(ctx *dataContext) error {
	if !ctx.ServiceProfile.NwkGeoLoc {
		log.WithFields(log.Fields{
			"dev_eui": ctx.DeviceSession.DevEUI,
		}).Debug("skipping geolocation, it is disabled by the service-profile")
		return nil
	}

	if geolocationserver.Client() == nil {
		log.WithFields(log.Fields{
			"dev_eui": ctx.DeviceSession.DevEUI,
		}).Debug("skipping geolocation, no client configured")
		return nil
	}

	var rxInfoWithFineTimestamp []*gw.UplinkRXInfo
	for i := range ctx.RXPacket.RXInfoSet {
		if ctx.RXPacket.RXInfoSet[i].FineTimestampType == gw.FineTimestampType_PLAIN {
			rxInfoWithFineTimestamp = append(rxInfoWithFineTimestamp, ctx.RXPacket.RXInfoSet[i])
		}
	}

	if len(rxInfoWithFineTimestamp) < 3 {
		log.WithFields(log.Fields{
			"dev_eui": ctx.DeviceSession.DevEUI,
		}).Debug("skipping geolocation, not enough gateway meta-data")
		return nil
	}

	// perform the actual geolocation in a separate goroutine
	go func(devEUI lorawan.EUI64, referenceAlt float64, geoClient geo.GeolocationServerServiceClient, asClient as.ApplicationServerServiceClient, rxInfo []*gw.UplinkRXInfo) {
		resp, err := geoClient.ResolveTDOA(context.Background(), &geo.ResolveTDOARequest{
			DevEui: devEUI[:],
			FrameRxInfo: &geo.FrameRXInfo{
				RxInfo: rxInfo,
			},
			DeviceReferenceAltitude: referenceAlt,
		})
		if err != nil {
			log.WithFields(log.Fields{
				"dev_eui": devEUI,
			}).WithError(err).Error("resolve tdoa error")
			return
		}

		if resp.Result == nil || resp.Result.Location == nil {
			log.WithFields(log.Fields{
				"dev_eui": devEUI,
			}).Error("geolocation-server result or result.location must not be nil")
			return
		}

		_, err = asClient.SetDeviceLocation(context.Background(), &as.SetDeviceLocationRequest{
			DevEui:   devEUI[:],
			Location: resp.Result.Location,
		})
		if err != nil {
			log.WithFields(log.Fields{
				"dev_eui": devEUI,
			}).WithError(err).Error("set device-location error")
		}

	}(ctx.DeviceSession.DevEUI, ctx.DeviceSession.ReferenceAltitude, geolocationserver.Client(), ctx.ApplicationServerClient, rxInfoWithFineTimestamp)

	return nil
}

func decryptFOptsMACCommands(ctx *dataContext) error {
	if ctx.DeviceSession.GetMACVersion() == lorawan.LoRaWAN1_0 {
		if err := ctx.RXPacket.PHYPayload.DecodeFOptsToMACCommands(); err != nil {
			return errors.Wrap(err, "decode fOpts to mac-commands error")
		}
	} else {
		if err := ctx.RXPacket.PHYPayload.DecryptFOpts(ctx.DeviceSession.NwkSEncKey); err != nil {
			return errors.Wrap(err, "decrypt fOpts mac-commands error")
		}
	}
	return nil
}

func decryptFRMPayloadMACCommands(ctx *dataContext) error {
	// only decrypt when FPort is equal to 0
	if ctx.MACPayload.FPort != nil && *ctx.MACPayload.FPort == 0 {
		if err := ctx.RXPacket.PHYPayload.DecryptFRMPayload(ctx.DeviceSession.NwkSEncKey); err != nil {
			return errors.Wrap(err, "decrypt FRMPayload error")
		}
	}

	return nil
}

func setBeaconLocked(ctx *dataContext) error {
	// set the Class-B beacon locked
	if ctx.DeviceSession.BeaconLocked == ctx.MACPayload.FHDR.FCtrl.ClassB {
		// no state change
		return nil
	}

	ctx.DeviceSession.BeaconLocked = ctx.MACPayload.FHDR.FCtrl.ClassB

	if ctx.DeviceSession.BeaconLocked {
		d, err := storage.GetDevice(storage.DB(), ctx.DeviceSession.DevEUI)
		if err != nil {
			return errors.Wrap(err, "get device")
		}
		d.Mode = storage.DeviceModeB
		if err := storage.UpdateDevice(storage.DB(), &d); err != nil {
			return errors.Wrap(err, "update device error")
		}

		if err := classb.ScheduleDeviceQueueToPingSlotsForDevEUI(storage.DB(), ctx.DeviceProfile, ctx.DeviceSession); err != nil {
			return errors.Wrap(err, "schedule device-queue to ping-slots error")
		}

		log.WithFields(log.Fields{
			"dev_eui": ctx.DeviceSession.DevEUI,
			"mode":    storage.DeviceModeB,
		}).Info("device changed mode")
	} else {
		d, err := storage.GetDevice(storage.DB(), ctx.DeviceSession.DevEUI)
		if err != nil {
			return errors.Wrap(err, "get device")
		}
		d.Mode = storage.DeviceModeA
		if err := storage.UpdateDevice(storage.DB(), &d); err != nil {
			return errors.Wrap(err, "update device error")
		}

		log.WithFields(log.Fields{
			"dev_eui": ctx.DeviceSession.DevEUI,
			"mode":    storage.DeviceModeA,
		}).Info("device changed mode")
	}

	return nil
}

func sendRXInfoToNetworkController(ctx *dataContext) error {
	// TODO: change so that errors get logged but not returned
	if err := sendRXInfoPayload(ctx.DeviceSession, ctx.RXPacket); err != nil {
		return errors.Wrap(err, "send rx-info to network-controller error")
	}

	return nil
}

func handleFOptsMACCommands(ctx *dataContext) error {
	if len(ctx.MACPayload.FHDR.FOpts) == 0 {
		return nil
	}

	blocks, mustRespondWithDownlink, err := handleUplinkMACCommands(
		&ctx.DeviceSession,
		ctx.DeviceProfile,
		ctx.ServiceProfile,
		ctx.ApplicationServerClient,
		ctx.MACPayload.FHDR.FOpts,
		ctx.RXPacket,
	)
	if err != nil {
		log.WithFields(log.Fields{
			"dev_eui": ctx.DeviceSession.DevEUI,
			"fopts":   ctx.MACPayload.FHDR.FOpts,
		}).Errorf("handle FOpts mac commands error: %s", err)
		return nil
	}

	ctx.MACCommandResponses = append(ctx.MACCommandResponses, blocks...)
	if !ctx.MustSendDownlink {
		ctx.MustSendDownlink = mustRespondWithDownlink
	}

	return nil
}

func handleFRMPayloadMACCommands(ctx *dataContext) error {
	if ctx.MACPayload.FPort == nil || *ctx.MACPayload.FPort != 0 {
		return nil
	}

	if len(ctx.MACPayload.FRMPayload) == 0 {
		return errors.New("expected mac commands, but FRMPayload is empty (FPort=0)")
	}

	blocks, mustRespondWithDownlink, err := handleUplinkMACCommands(&ctx.DeviceSession, ctx.DeviceProfile, ctx.ServiceProfile, ctx.ApplicationServerClient, ctx.MACPayload.FRMPayload, ctx.RXPacket)
	if err != nil {
		log.WithFields(log.Fields{
			"dev_eui":  ctx.DeviceSession.DevEUI,
			"commands": ctx.MACPayload.FRMPayload,
		}).Errorf("handle FRMPayload mac commands error: %s", err)
		return nil
	}

	ctx.MACCommandResponses = append(ctx.MACCommandResponses, blocks...)
	if !ctx.MustSendDownlink {
		ctx.MustSendDownlink = mustRespondWithDownlink
	}

	return nil
}

func sendFRMPayloadToApplicationServer(ctx *dataContext) error {
	publishDataUpReq := as.HandleUplinkDataRequest{
		DevEui:  ctx.DeviceSession.DevEUI[:],
		JoinEui: ctx.DeviceSession.JoinEUI[:],
		FCnt:    ctx.MACPayload.FHDR.FCnt,
		Adr:     ctx.MACPayload.FHDR.FCtrl.ADR,
		TxInfo:  ctx.RXPacket.TXInfo,
	}

	dr, err := helpers.GetDataRateIndex(true, ctx.RXPacket.TXInfo, band.Band())
	if err != nil {
		errors.Wrap(err, "get data-rate error")
	}
	publishDataUpReq.Dr = uint32(dr)

	if ctx.DeviceSession.AppSKeyEvelope != nil {
		publishDataUpReq.DeviceActivationContext = &as.DeviceActivationContext{
			DevAddr: ctx.DeviceSession.DevAddr[:],
			AppSKey: &common.KeyEnvelope{
				KekLabel: ctx.DeviceSession.AppSKeyEvelope.KEKLabel,
				AesKey:   ctx.DeviceSession.AppSKeyEvelope.AESKey,
			},
		}

		ctx.DeviceSession.AppSKeyEvelope = nil
	}

	if ctx.ServiceProfile.AddGWMetadata {
		publishDataUpReq.RxInfo = ctx.RXPacket.RXInfoSet
	}

	if ctx.MACPayload.FPort != nil {
		publishDataUpReq.FPort = uint32(*ctx.MACPayload.FPort)
	}

	// The DataPayload is only used for FPort != 0 (or nil)
	if ctx.MACPayload.FPort != nil && *ctx.MACPayload.FPort != 0 && len(ctx.MACPayload.FRMPayload) == 1 {
		dataPL, ok := ctx.MACPayload.FRMPayload[0].(*lorawan.DataPayload)
		if !ok {
			return fmt.Errorf("expected type *lorawan.DataPayload, got %T", ctx.MACPayload.FRMPayload[0])
		}
		publishDataUpReq.Data = dataPL.Bytes
	}

	go func(asClient as.ApplicationServerServiceClient, publishDataUpReq as.HandleUplinkDataRequest) {
		ctx := context.Background()
		ctxTimeout, cancel := context.WithTimeout(ctx, applicationClientTimeout)
		defer cancel()

		if _, err := asClient.HandleUplinkData(ctxTimeout, &publishDataUpReq); err != nil {
			log.WithError(err).Error("publish uplink data to application-server error")
		}
	}(ctx.ApplicationServerClient, publishDataUpReq)

	return nil
}

func setLastRXInfoSet(ctx *dataContext) error {
	if len(ctx.RXPacket.RXInfoSet) != 0 {
		gatewayID := helpers.GetGatewayID(ctx.RXPacket.RXInfoSet[0])
		ctx.DeviceSession.UplinkGatewayHistory = map[lorawan.EUI64]storage.UplinkGatewayHistory{
			gatewayID: storage.UplinkGatewayHistory{},
		}
	}
	return nil
}

func syncUplinkFCnt(ctx *dataContext) error {
	// sync counter with that of the device + 1
	ctx.DeviceSession.FCntUp = ctx.MACPayload.FHDR.FCnt + 1
	return nil
}

func saveDeviceSession(ctx *dataContext) error {
	// save node-session
	return storage.SaveDeviceSession(storage.RedisPool(), ctx.DeviceSession)
}

func handleUplinkACK(ctx *dataContext) error {
	if !ctx.MACPayload.FHDR.FCtrl.ACK {
		return nil
	}

	qi, err := storage.GetPendingDeviceQueueItemForDevEUI(storage.DB(), ctx.DeviceSession.DevEUI)
	if err != nil {
		log.WithFields(log.Fields{
			"dev_eui": ctx.DeviceSession.DevEUI,
		}).WithError(err).Error("get device-queue item error")
		return nil
	}
	if qi.FCnt != ctx.DeviceSession.NFCntDown-1 {
		log.WithFields(log.Fields{
			"dev_eui":                  ctx.DeviceSession.DevEUI,
			"device_queue_item_fcnt":   qi.FCnt,
			"device_session_fcnt_down": ctx.DeviceSession.NFCntDown,
		}).Error("frame-counter of device-queue item out of sync with device-session")
		return nil
	}

	if err := storage.DeleteDeviceQueueItem(storage.DB(), qi.ID); err != nil {
		return errors.Wrap(err, "delete device-queue item error")
	}

	_, err = ctx.ApplicationServerClient.HandleDownlinkACK(context.Background(), &as.HandleDownlinkACKRequest{
		DevEui:       ctx.DeviceSession.DevEUI[:],
		FCnt:         qi.FCnt,
		Acknowledged: true,
	})
	if err != nil {
		return errors.Wrap(err, "application-server client error")
	}

	return nil
}

func handleDownlink(ctx *dataContext) error {
	// handle downlink (ACK)
	time.Sleep(getDownlinkDataDelay)
	if err := datadown.HandleResponse(
		ctx.RXPacket,
		ctx.ServiceProfile,
		ctx.DeviceSession,
		ctx.MACPayload.FHDR.FCtrl.ADR,
		ctx.MACPayload.FHDR.FCtrl.ADRACKReq || ctx.MustSendDownlink,
		ctx.RXPacket.PHYPayload.MHDR.MType == lorawan.ConfirmedDataUp,
		ctx.MACCommandResponses,
	); err != nil {
		return errors.Wrap(err, "run uplink response flow error")
	}

	return nil
}

// sendRXInfoPayload sends the rx and tx meta-data to the network controller.
func sendRXInfoPayload(ds storage.DeviceSession, rxPacket models.RXPacket) error {
	rxInfoReq := nc.HandleUplinkMetaDataRequest{
		DevEui: ds.DevEUI[:],
		TxInfo: rxPacket.TXInfo,
		RxInfo: rxPacket.RXInfoSet,
	}

	_, err := controller.Client().HandleUplinkMetaData(context.Background(), &rxInfoReq)
	if err != nil {
		return fmt.Errorf("publish rxinfo to network-controller error: %s", err)
	}
	log.WithFields(log.Fields{
		"dev_eui": ds.DevEUI,
	}).Info("rx info sent to network-controller")
	return nil
}

// handleUplinkMACCommands handles the given uplink mac-commands.
// It returns the mac-commands to respond with + a bool indicating the a downlink MUST be send,
// this to make sure that a response has been received by the NS.
func handleUplinkMACCommands(ds *storage.DeviceSession, dp storage.DeviceProfile, sp storage.ServiceProfile, asClient as.ApplicationServerServiceClient, commands []lorawan.Payload, rxPacket models.RXPacket) ([]storage.MACCommandBlock, bool, error) {
	var cids []lorawan.CID
	var out []storage.MACCommandBlock
	var mustRespondWithDownlink bool
	blocks := make(map[lorawan.CID]storage.MACCommandBlock)

	// group mac-commands by CID
	for _, pl := range commands {
		cmd, ok := pl.(*lorawan.MACCommand)
		if !ok {
			return nil, false, fmt.Errorf("expected *lorawan.MACCommand, got %T", pl)
		}
		if cmd == nil {
			return nil, false, errors.New("*lorawan.MACCommand must not be nil")
		}

		block, ok := blocks[cmd.CID]
		if !ok {
			block = storage.MACCommandBlock{
				CID: cmd.CID,
			}
			cids = append(cids, cmd.CID)
		}
		block.MACCommands = append(block.MACCommands, *cmd)
		blocks[cmd.CID] = block
	}

	for _, cid := range cids {
		switch cid {
		case lorawan.RXTimingSetupAns:
			// From the specs:
			// The RXTimingSetupAns command should be added in the FOpt field of all uplinks until a
			// class A downlink is received by the end-device.
			mustRespondWithDownlink = true
		default:
			// nothing to do
		}

		block := blocks[cid]

		logFields := log.Fields{
			"dev_eui": ds.DevEUI,
			"cid":     block.CID,
		}

		var external bool

		if !disableMACCommands {
			// read pending mac-command block for CID. e.g. on case of an ack, the
			// pending mac-command block contains the request.
			// we need this pending mac-command block to find out if the command
			// was scheduled through the API (external).
			pending, err := storage.GetPendingMACCommand(storage.RedisPool(), ds.DevEUI, block.CID)
			if err != nil {
				log.WithFields(logFields).Errorf("read pending mac-command error: %s", err)
				continue
			}
			if pending != nil {
				external = pending.External
			}

			// in case the node is requesting a mac-command, there is nothing pending
			if pending != nil {
				if err = storage.DeletePendingMACCommand(storage.RedisPool(), ds.DevEUI, block.CID); err != nil {
					log.WithFields(logFields).Errorf("delete pending mac-command error: %s", err)
				}
			}

			// CID >= 0x80 are proprietary mac-commands and are not handled by LoRa Server
			if block.CID < 0x80 {
				responseBlocks, err := maccommand.Handle(ds, dp, sp, asClient, block, pending, rxPacket)
				if err != nil {
					log.WithFields(logFields).Errorf("handle mac-command block error: %s", err)
				} else {
					out = append(out, responseBlocks...)
				}
			}
		}

		// Report to external controller:
		//  * in case of proprietary mac-commands
		//  * in case when the request has been scheduled through the API
		//  * in case mac-commands are disabled in the LoRa Server configuration
		if disableMACCommands || block.CID >= 0x80 || external {
			var data [][]byte
			for _, cmd := range block.MACCommands {
				b, err := cmd.MarshalBinary()
				if err != nil {
					log.WithFields(logFields).Errorf("marshal mac-command to binary error: %s", err)
					continue
				}
				data = append(data, b)
			}
			_, err := controller.Client().HandleUplinkMACCommand(context.Background(), &nc.HandleUplinkMACCommandRequest{
				DevEui:   ds.DevEUI[:],
				Cid:      uint32(block.CID),
				Commands: data,
			})
			if err != nil {
				log.WithFields(logFields).Errorf("send mac-command to network-controller error: %s", err)
			} else {
				log.WithFields(logFields).Info("mac-command sent to network-controller")
			}
		}
	}

	return out, mustRespondWithDownlink, nil
}
