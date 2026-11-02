// File overview: Which zone an airport's clocks are in.
//
// A flight's times are printed in the local time of the airport they belong to,
// and the structured sources often leave the offset off. The airport then says
// which zone was meant -- it is the one fact in a booking that cannot be read
// two ways. An airport this table does not know leaves the time floating rather
// than guessed: a flight shown at the right wall-clock time in the wrong zone is
// recoverable, one moved by a guessed offset is not.

package mailparse

import (
	"strings"
	"time"
)

// airportZones maps IATA codes to IANA zones for the airports a European
// reader's bookings name most. It is a list of what is common, not a copy of an
// airport database; a miss costs a floating time, never a wrong one.
var airportZones = map[string]string{
	// Germany
	"BER": "Europe/Berlin", "BRE": "Europe/Berlin", "CGN": "Europe/Berlin", "DRS": "Europe/Berlin",
	"DTM": "Europe/Berlin", "DUS": "Europe/Berlin", "ERF": "Europe/Berlin", "FDH": "Europe/Berlin",
	"FKB": "Europe/Berlin", "FMM": "Europe/Berlin", "FMO": "Europe/Berlin", "FRA": "Europe/Berlin",
	"HAJ": "Europe/Berlin", "HAM": "Europe/Berlin", "HHN": "Europe/Berlin", "KSF": "Europe/Berlin",
	"LEJ": "Europe/Berlin", "MUC": "Europe/Berlin", "NRN": "Europe/Berlin", "NUE": "Europe/Berlin",
	"PAD": "Europe/Berlin", "RLG": "Europe/Berlin", "SCN": "Europe/Berlin", "STR": "Europe/Berlin",
	"SXF": "Europe/Berlin", "TXL": "Europe/Berlin",
	// Austria, Switzerland
	"GRZ": "Europe/Vienna", "INN": "Europe/Vienna", "KLU": "Europe/Vienna", "LNZ": "Europe/Vienna",
	"SZG": "Europe/Vienna", "VIE": "Europe/Vienna",
	"BSL": "Europe/Zurich", "GVA": "Europe/Zurich", "ZRH": "Europe/Zurich",
	// Benelux, France
	"AMS": "Europe/Amsterdam", "EIN": "Europe/Amsterdam", "RTM": "Europe/Amsterdam",
	"BRU": "Europe/Brussels", "CRL": "Europe/Brussels", "LUX": "Europe/Luxembourg",
	"BIQ": "Europe/Paris", "BOD": "Europe/Paris", "LYS": "Europe/Paris", "MRS": "Europe/Paris",
	"NCE": "Europe/Paris", "NTE": "Europe/Paris", "ORY": "Europe/Paris", "CDG": "Europe/Paris",
	"SXB": "Europe/Paris", "TLS": "Europe/Paris", "AJA": "Europe/Paris", "BIA": "Europe/Paris",
	// British Isles
	"BHX": "Europe/London", "BRS": "Europe/London", "EDI": "Europe/London", "GLA": "Europe/London",
	"LCY": "Europe/London", "LGW": "Europe/London", "LHR": "Europe/London", "LTN": "Europe/London",
	"MAN": "Europe/London", "STN": "Europe/London", "BFS": "Europe/London",
	"DUB": "Europe/Dublin", "ORK": "Europe/Dublin", "SNN": "Europe/Dublin",
	// Iberia
	"AGP": "Europe/Madrid", "ALC": "Europe/Madrid", "BCN": "Europe/Madrid", "BIO": "Europe/Madrid",
	"IBZ": "Europe/Madrid", "MAD": "Europe/Madrid", "PMI": "Europe/Madrid", "SVQ": "Europe/Madrid",
	"VLC": "Europe/Madrid", "MAH": "Europe/Madrid",
	"ACE": "Atlantic/Canary", "FUE": "Atlantic/Canary", "LPA": "Atlantic/Canary", "TFN": "Atlantic/Canary",
	"TFS": "Atlantic/Canary", "SPC": "Atlantic/Canary",
	"FAO": "Europe/Lisbon", "LIS": "Europe/Lisbon", "OPO": "Europe/Lisbon", "FNC": "Atlantic/Madeira",
	"PDL": "Atlantic/Azores",
	// Italy, Malta
	"BGY": "Europe/Rome", "BLQ": "Europe/Rome", "BRI": "Europe/Rome", "CAG": "Europe/Rome",
	"CTA": "Europe/Rome", "FCO": "Europe/Rome", "FLR": "Europe/Rome", "LIN": "Europe/Rome",
	"MXP": "Europe/Rome", "NAP": "Europe/Rome", "OLB": "Europe/Rome", "PMO": "Europe/Rome",
	"PSA": "Europe/Rome", "TRN": "Europe/Rome", "VCE": "Europe/Rome", "VRN": "Europe/Rome",
	"CIA": "Europe/Rome", "MLA": "Europe/Malta",
	// Nordics, Baltics
	"ARN": "Europe/Stockholm", "GOT": "Europe/Stockholm", "BMA": "Europe/Stockholm",
	"CPH": "Europe/Copenhagen", "BLL": "Europe/Copenhagen", "AAL": "Europe/Copenhagen",
	"OSL": "Europe/Oslo", "BGO": "Europe/Oslo", "TRD": "Europe/Oslo", "SVG": "Europe/Oslo",
	"HEL": "Europe/Helsinki", "KEF": "Atlantic/Reykjavik",
	"RIX": "Europe/Riga", "TLL": "Europe/Tallinn", "VNO": "Europe/Vilnius",
	// Central and Eastern Europe
	"PRG": "Europe/Prague", "BUD": "Europe/Budapest", "WAW": "Europe/Warsaw", "KRK": "Europe/Warsaw",
	"GDN": "Europe/Warsaw", "WRO": "Europe/Warsaw", "KTW": "Europe/Warsaw", "POZ": "Europe/Warsaw",
	"BTS": "Europe/Bratislava", "LJU": "Europe/Ljubljana", "ZAG": "Europe/Zagreb",
	"SPU": "Europe/Zagreb", "DBV": "Europe/Zagreb", "BEG": "Europe/Belgrade", "OTP": "Europe/Bucharest",
	"SOF": "Europe/Sofia", "VAR": "Europe/Sofia", "BOJ": "Europe/Sofia", "KIV": "Europe/Chisinau",
	"TIA": "Europe/Tirane", "SKP": "Europe/Skopje", "SJJ": "Europe/Sarajevo", "TGD": "Europe/Podgorica",
	// Greece, Cyprus, Turkey
	"ATH": "Europe/Athens", "CFU": "Europe/Athens", "CHQ": "Europe/Athens", "HER": "Europe/Athens",
	"JMK": "Europe/Athens", "JTR": "Europe/Athens", "KGS": "Europe/Athens", "RHO": "Europe/Athens",
	"SKG": "Europe/Athens", "ZTH": "Europe/Athens", "LCA": "Asia/Nicosia", "PFO": "Asia/Nicosia",
	"IST": "Europe/Istanbul", "SAW": "Europe/Istanbul", "AYT": "Europe/Istanbul", "ADB": "Europe/Istanbul",
	"DLM": "Europe/Istanbul", "BJV": "Europe/Istanbul", "ESB": "Europe/Istanbul",
	// Middle East, Africa
	"DXB": "Asia/Dubai", "AUH": "Asia/Dubai", "DOH": "Asia/Qatar", "TLV": "Asia/Jerusalem",
	"AMM": "Asia/Amman", "CAI": "Africa/Cairo", "HRG": "Africa/Cairo", "SSH": "Africa/Cairo",
	"RMF": "Africa/Cairo", "CMN": "Africa/Casablanca", "RAK": "Africa/Casablanca", "AGA": "Africa/Casablanca",
	"TUN": "Africa/Tunis", "DJE": "Africa/Tunis", "NBE": "Africa/Tunis", "JNB": "Africa/Johannesburg",
	"CPT": "Africa/Johannesburg", "NBO": "Africa/Nairobi", "ZNZ": "Africa/Dar_es_Salaam",
	"MRU": "Indian/Mauritius", "SEZ": "Indian/Mahe", "MLE": "Indian/Maldives",
	// Asia, Oceania
	"BKK": "Asia/Bangkok", "HKT": "Asia/Bangkok", "SIN": "Asia/Singapore", "KUL": "Asia/Kuala_Lumpur",
	"HKG": "Asia/Hong_Kong", "PEK": "Asia/Shanghai", "PVG": "Asia/Shanghai", "NRT": "Asia/Tokyo",
	"HND": "Asia/Tokyo", "KIX": "Asia/Tokyo", "ICN": "Asia/Seoul", "DEL": "Asia/Kolkata",
	"BOM": "Asia/Kolkata", "DPS": "Asia/Makassar", "CGK": "Asia/Jakarta", "MNL": "Asia/Manila",
	"SGN": "Asia/Ho_Chi_Minh", "HAN": "Asia/Ho_Chi_Minh", "CMB": "Asia/Colombo", "TPE": "Asia/Taipei",
	"SYD": "Australia/Sydney", "MEL": "Australia/Melbourne", "BNE": "Australia/Brisbane",
	"PER": "Australia/Perth", "AKL": "Pacific/Auckland",
	// Americas
	"ATL": "America/New_York", "BOS": "America/New_York", "EWR": "America/New_York", "IAD": "America/New_York",
	"JFK": "America/New_York", "LGA": "America/New_York", "MIA": "America/New_York", "MCO": "America/New_York",
	"PHL": "America/New_York", "CLT": "America/New_York", "DTW": "America/Detroit", "ORD": "America/Chicago",
	"DFW": "America/Chicago", "IAH": "America/Chicago", "MSP": "America/Chicago", "DEN": "America/Denver",
	"PHX": "America/Phoenix", "LAS": "America/Los_Angeles", "LAX": "America/Los_Angeles",
	"SFO": "America/Los_Angeles", "SEA": "America/Los_Angeles", "SAN": "America/Los_Angeles",
	"HNL": "Pacific/Honolulu", "YYZ": "America/Toronto", "YUL": "America/Toronto", "YVR": "America/Vancouver",
	"YYC": "America/Edmonton", "MEX": "America/Mexico_City", "CUN": "America/Cancun", "GRU": "America/Sao_Paulo",
	"GIG": "America/Sao_Paulo", "EZE": "America/Argentina/Buenos_Aires", "BOG": "America/Bogota",
	"LIM": "America/Lima", "SCL": "America/Santiago", "PUJ": "America/Santo_Domingo", "HAV": "America/Havana",
}

// airportZone returns the zone an airport's clocks are in, or nil when the code
// is not one this table knows.
func airportZone(code string) *time.Location {
	name, ok := airportZones[strings.ToUpper(strings.TrimSpace(code))]
	if !ok {
		return nil
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil
	}
	return loc
}
