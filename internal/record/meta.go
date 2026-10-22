package record

// Record formats.
const (
	// FormatPrototype is the format the prototype (legacy/sysbackup.py) wrote. Carryon no longer
	// reads it.
	FormatPrototype = 1
	// FormatCurrent is the format Carryon writes: several users, machine name, distribution.
	FormatCurrent = 2
)

// Meta is what the record says about the machine it describes.
//
// Fields are declared in alphabetical order of their JSON names, like Item.
type Meta struct {
	Carryon     string `json:"carryon,omitempty"`      // Carryon version that wrote the record
	Chassis     string `json:"chassis"`                // hostnamectl chassis: desktop, laptop...
	Complete    bool   `json:"complete"`               // captured as root with nothing skipped
	Distro      string `json:"distro,omitempty"`       // os-release ID
	Format      int    `json:"format"`                 // FormatCurrent
	Home        string `json:"home"`                   // the main user's home
	Host        string `json:"host"`                   // hostname at capture time
	Kernel      string `json:"kernel"`                 // running kernel release
	Machine     string `json:"machine"`                // hardware fingerprint ("" when captured without root)
	MachineName string `json:"machine_name,omitempty"` // name the snapshots are stored under
	User        string `json:"user"`                   // the main user
	Users       []User `json:"users,omitempty"`        // every user in the record
}

// User is one user whose home and settings are in the record.
type User struct {
	Home string `json:"home"`
	Name string `json:"name"`
}

// AllUsers returns every user in the record. A record that lists no users but names a main user
// has just that one.
func (m Meta) AllUsers() []User {
	if len(m.Users) > 0 {
		return m.Users
	}
	if m.User == "" {
		return nil
	}
	return []User{{Name: m.User, Home: m.Home}}
}

// Name is the name the machine's snapshots are stored under: the machine name, or the hostname
// when the record has none.
func (m Meta) Name() string {
	if m.MachineName != "" {
		return m.MachineName
	}
	return m.Host
}
