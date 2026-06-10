package protocol

// JT808 message IDs (terminal → platform direction)
const (
	MsgTerminalResp   uint16 = 0x0001 // terminal general response (ACK to server)
	MsgHeartbeat      uint16 = 0x0002 // keepalive, empty body
	MsgRegistration   uint16 = 0x0100 // device registers; server issues auth token
	MsgAuthentication uint16 = 0x0102 // device authenticates with token
	MsgTerminalProps   uint16 = 0x0120 // terminal properties report (JT808-2017 extension)
	MsgUpgradeResult   uint16 = 0x0121 // terminal upgrade / control result notification (sent at startup; empty body = none pending)
	MsgACCStatusReport uint16 = 0x0304 // ACC/relay status report (Concox dialect; body[0]: 0x00=off 0x01=on)
	MsgLocationReport  uint16 = 0x0200 // single GPS fix
	MsgBatchLocation   uint16 = 0x0704 // multiple GPS fixes in one packet
)

// JT808 message IDs (platform → terminal direction)
const (
	MsgPlatformResp     uint16 = 0x8001 // platform general response (ACK to device)
	MsgRegistrationResp uint16 = 0x8100 // registration response + auth token
	MsgSetParams        uint16 = 0x8103 // set terminal parameters
	MsgPlatformQuery    uint16 = 0x8104 // query terminal parameters
	MsgTerminalCtrl     uint16 = 0x8105 // terminal control
	MsgDeleteLocation   uint16 = 0x8202 // delete stored location
	MsgQueryLocation    uint16 = 0x8201 // query device location (device must reply 0x0200)
)

// Registration result codes (in 0x8100 response)
const (
	RegSuccess             byte = 0x00
	RegVehicleRegistered   byte = 0x01 // vehicle already in system
	RegVehicleNotInDB      byte = 0x02
	RegTerminalRegistered  byte = 0x03 // terminal already registered
	RegTerminalNotInDB     byte = 0x04
)

// Platform response result codes (in 0x8001 ACK)
const (
	ResultOK          byte = 0x00
	ResultFail        byte = 0x01
	ResultMsgError    byte = 0x02 // message has an error
	ResultUnsupported byte = 0x03
	ResultAlarmACK    byte = 0x04
)

// Alarm flag bits (LocationReport.AlarmFlags)
const (
	AlarmSOS              uint32 = 1 << 0
	AlarmOverspeed        uint32 = 1 << 1
	AlarmFatigueDriving   uint32 = 1 << 2
	AlarmLowBattery       uint32 = 1 << 7
	AlarmGNSSFault        uint32 = 1 << 8
	AlarmGNSSAntennaOpen  uint32 = 1 << 9
	AlarmGNSSAntennaShort uint32 = 1 << 10
	AlarmPowerLow         uint32 = 1 << 11
	AlarmPowerOff         uint32 = 1 << 12
	AlarmVibration        uint32 = 1 << 16
	AlarmIllegalIgnition  uint32 = 1 << 20
)

// Status flag bits (LocationReport.StatusFlags)
const (
	StatusACCOn    uint32 = 1 << 0 // ignition / power on
	StatusLocated  uint32 = 1 << 1 // GPS has a fix (0 = not located)
	StatusLatSouth uint32 = 1 << 2 // latitude direction: south (0 = north)
	StatusLonWest  uint32 = 1 << 3 // longitude direction: west (0 = east)
	StatusStopped  uint32 = 1 << 4 // vehicle stopped (0 = running)
)

// LocationReport is a decoded 0x0200 location message body.
type LocationReport struct {
	AlarmFlags  uint32
	StatusFlags uint32
	Latitude    float64 // degrees; negative = south
	Longitude   float64 // degrees; negative = west
	Altitude    uint16  // metres above sea level
	Speed       float64 // km/h (raw wire value is 0.1 km/h units)
	Direction   uint16  // 0–359 degrees from true north
	Timestamp   string  // ISO-8601 UTC, e.g. "2024-03-15T08:30:00Z"

	// Derived from StatusFlags for caller convenience
	GPSFixed bool
	ACCOn    bool

	// Additional info items from TLV trailer (id → raw bytes)
	Extras map[uint8][]byte
}

// HasAlarm returns true if any of the given alarm bits are set.
func (l *LocationReport) HasAlarm(mask uint32) bool {
	return l.AlarmFlags&mask != 0
}

// RegistrationInfo is a decoded 0x0100 registration message body.
type RegistrationInfo struct {
	ProvinceID  uint16
	CityID      uint16
	ManufID     string // 5-byte ASCII manufacturer code
	DeviceModel string // 20-byte ASCII
	DeviceID    string // 7-byte ASCII (often IMEI suffix)
	PlateColor  byte
	PlateNumber string
}
