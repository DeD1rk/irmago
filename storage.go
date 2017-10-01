package irmago

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io/ioutil"
	"os"
	"path"
	"time"

	"github.com/go-errors/errors"
	"github.com/mhe/gabi"
)

// Filenames in which we store stuff
const (
	skFile         = "sk"
	attributesFile = "attrs"
	kssFile        = "kss"
	paillierFile   = "paillier"
	updatesFile    = "updates"
	logsFile       = "logs"
	signaturesDir  = "sigs"
)

// PathExists checks if the specified path exists.
func PathExists(path string) (bool, error) {
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return true, err
}

func ensureDirectoryExists(path string) error {
	exists, err := PathExists(path)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	return os.Mkdir(path, 0700)
}

// Save the filecontents at the specified path atomically:
// - first save the content in a temp file with a random filename in the same dir
// - then rename the temp file to the specified filepath, overwriting the old file
func saveFile(filepath string, content []byte) (err error) {
	dir := path.Dir(filepath)

	// Read random data for filename and convert to hex
	randBytes := make([]byte, 16)
	_, err = rand.Read(randBytes)
	if err != nil {
		return
	}
	tempfilename := hex.EncodeToString(randBytes)

	// Create temp file
	err = ioutil.WriteFile(dir+"/"+tempfilename, content, 0600)
	if err != nil {
		return
	}

	// Rename, overwriting old file
	return os.Rename(dir+"/"+tempfilename, filepath)
}

// NewCredentialManager creates a new CredentialManager that uses the directory
// specified by storagePath for (de)serializing itself. irmaConfigurationPath
// is the path to a (possibly readonly) folder containing irma_configuration;
// androidStoragePath is an optional path to the files of the old android app
// (specify "" if you do not want to parse the old android app files),
// and keyshareHandler is used for when a registration to a keyshare server needs
// to happen.
// The credential manager returned by this function has been fully deserialized
// and is ready for use.
//
// NOTE: It is the responsibility of the caller that there exists a directory
// at storagePath!
func NewCredentialManager(
	storagePath string,
	irmaConfigurationPath string,
	androidStoragePath string,
	keyshareHandler KeyshareHandler,
) (*CredentialManager, error) {
	var err error
	cm := &CredentialManager{
		credentials:           make(map[CredentialTypeIdentifier]map[int]*credential),
		keyshareServers:       make(map[SchemeManagerIdentifier]*keyshareServer),
		attributes:            make(map[CredentialTypeIdentifier][]*AttributeList),
		irmaConfigurationPath: irmaConfigurationPath,
		androidStoragePath:    androidStoragePath,
		ConfigurationStore:    NewConfigurationStore(storagePath + "/irma_configuration"),
	}

	exists, err := PathExists(cm.irmaConfigurationPath)
	if !exists {
		return nil, errors.New("irmaConfigurationPath does not exist")
	}

	// Ensure storage path exists, and populate it with necessary files
	cm.storagePath = storagePath
	if err = cm.ensureStorageExists(); err != nil {
		return nil, err
	}
	if err = cm.ConfigurationStore.ParseFolder(); err != nil {
		return nil, err
	}

	// Perform new update functions from credentialManagerUpdates, if any
	if err = cm.update(); err != nil {
		return nil, err
	}

	// Load our stuff
	if cm.secretkey, err = cm.loadSecretKey(); err != nil {
		return nil, err
	}
	if cm.attributes, err = cm.loadAttributes(); err != nil {
		return nil, err
	}
	if cm.paillierKeyCache, err = cm.loadPaillierKeys(); err != nil {
		return nil, err
	}
	if cm.keyshareServers, err = cm.loadKeyshareServers(); err != nil {
		return nil, err
	}

	unenrolled := cm.unenrolledKeyshareServers()
	switch len(unenrolled) {
	case 0: // nop
	case 1:
		if keyshareHandler == nil {
			return nil, errors.New("Keyshare server found but no KeyshareHandler was given")
		}
		keyshareHandler.StartRegistration(unenrolled[0], func(email, pin string) {
			cm.KeyshareEnroll(unenrolled[0].Identifier(), email, pin)
		})
	default:
		return nil, errors.New("Too many keyshare servers")
	}

	return cm, nil
}

// update performs any function from credentialManagerUpdates that has not
// already been executed in the past, keeping track of previously executed updates
// in the file at updatesFile.
func (cm *CredentialManager) update() error {
	// Load and parse file containing info about already performed updates
	exists, err := PathExists(cm.path(updatesFile))
	if err != nil {
		return err
	}
	if !exists {
		cm.updates = []update{}
	} else {
		bytes, err := ioutil.ReadFile(cm.path(updatesFile))
		if err != nil {
			return err
		}
		if err = json.Unmarshal(bytes, &cm.updates); err != nil {
			return err
		}
	}

	// Perform all new updates
	for i := len(cm.updates); i < len(credentialManagerUpdates); i++ {
		err = credentialManagerUpdates[i](cm)
		update := update{
			When:    Timestamp(time.Now()),
			Number:  i,
			Success: err == nil,
		}
		if err != nil {
			str := err.Error()
			update.Error = &str
		}
		cm.updates = append(cm.updates, update)
	}

	// Save updates file
	bytes, err := json.Marshal(cm.updates)
	if err != nil {
		return err
	}
	saveFile(cm.path(updatesFile), bytes)

	return nil
}

func (cm *CredentialManager) path(file string) string {
	return cm.storagePath + "/" + file
}

func (cm *CredentialManager) signatureFilename(attrs *AttributeList) string {
	// We take the SHA256 hash over all attributes as the filename for the signature.
	// This means that the signatures of two credentials that have identical attributes
	// will be written to the same file, one overwriting the other - but that doesn't
	// matter, because either one of the signatures is valid over both attribute lists,
	// so keeping one of them suffices.
	return cm.path(signaturesDir) + "/" + attrs.hash()
}

// ensureStorageExists initializes the credential storage folder,
// ensuring that it is in a usable state.
// NOTE: we do not create the folder if it does not exist!
// Setting it up in a properly protected location (e.g., with automatic
// backups to iCloud/Google disabled) is the responsibility of the user.
func (cm *CredentialManager) ensureStorageExists() error {
	exist, err := PathExists(cm.storagePath)
	if err != nil {
		return err
	}
	if !exist {
		return errors.New("credential storage path does not exist")
	}

	if exist, err = PathExists(cm.ConfigurationStore.path); err != nil {
		return err
	}
	if !exist {
		if err = ensureDirectoryExists(cm.ConfigurationStore.path); err != nil {
			return err
		}
		cm.ConfigurationStore.Copy(cm.irmaConfigurationPath, false)
	}
	return ensureDirectoryExists(cm.path(signaturesDir))
}

func (cm *CredentialManager) storeSecretKey(sk *secretKey) error {
	bytes, err := json.Marshal(sk)
	if err != nil {
		return err
	}
	return saveFile(cm.path(skFile), bytes)
}

func (cm *CredentialManager) storeSignature(cred *credential, counter int) (err error) {
	if cred.CredentialType() == nil {
		return errors.New("cannot add unknown credential type")
	}

	credbytes, err := json.Marshal(cred.Signature)
	if err != nil {
		return err
	}

	filename := cm.signatureFilename(cred.AttributeList())
	err = saveFile(filename, credbytes)
	return
}

func (cm *CredentialManager) storeAttributes() error {
	temp := []*AttributeList{}
	for _, attrlistlist := range cm.attributes {
		for _, attrlist := range attrlistlist {
			temp = append(temp, attrlist)
		}
	}

	if attrbytes, err := json.Marshal(temp); err == nil {
		err = saveFile(cm.path(attributesFile), attrbytes)
		return nil
	} else {
		return err
	}
}

func (cm *CredentialManager) storeKeyshareServers() (err error) {
	bts, err := json.Marshal(cm.keyshareServers)
	if err != nil {
		return
	}
	err = saveFile(cm.path(kssFile), bts)
	return
}

func (cm *CredentialManager) storePaillierKeys() (err error) {
	bts, err := json.Marshal(cm.paillierKeyCache)
	if err != nil {
		return
	}
	err = saveFile(cm.path(paillierFile), bts)
	return
}

func (cm *CredentialManager) storeLogs() (err error) {
	bts, err := json.Marshal(cm.logs)
	if err != nil {
		return
	}
	err = saveFile(cm.path(logsFile), bts)
	return
}

func (cm *CredentialManager) loadSignature(attrs *AttributeList) (signature *gabi.CLSignature, err error) {
	sigpath := cm.signatureFilename(attrs)
	exists, err := PathExists(sigpath)
	if err != nil {
		return
	}
	if !exists {
		return nil, errors.New("Signature file not found")
	}
	bytes, err := ioutil.ReadFile(sigpath)
	if err != nil {
		return
	}
	signature = new(gabi.CLSignature)
	err = json.Unmarshal(bytes, signature)
	return
}

// loadSecretKey retrieves and returns the secret key from storage, or if no secret key
// was found in storage, it generates, saves, and returns a new secret key.
func (cm *CredentialManager) loadSecretKey() (*secretKey, error) {
	sk := &secretKey{}
	var err error
	exists, err := PathExists(cm.path(skFile))
	if err != nil {
		return nil, err
	}
	if exists {
		var bytes []byte
		if bytes, err = ioutil.ReadFile(cm.path(skFile)); err != nil {
			return nil, err
		}
		if err = json.Unmarshal(bytes, sk); err != nil {
			return nil, err
		}
		return sk, err
	}

	sk, err = cm.generateSecretKey()
	if err != nil {
		return nil, err
	}
	err = cm.storeSecretKey(sk)
	if err != nil {
		return nil, err
	}
	return sk, nil
}

func (cm *CredentialManager) loadAttributes() (list map[CredentialTypeIdentifier][]*AttributeList, err error) {
	exists, err := PathExists(cm.path(attributesFile))
	if err != nil || !exists {
		return
	}
	bytes, err := ioutil.ReadFile(cm.path(attributesFile))
	if err != nil {
		return nil, err
	}

	// The attributes are stored as a list of instances of AttributeList
	temp := []*AttributeList{}
	list = make(map[CredentialTypeIdentifier][]*AttributeList)
	if err = json.Unmarshal(bytes, &temp); err != nil {
		return nil, err
	}
	for _, attrlist := range temp {
		attrlist.MetadataAttribute = MetadataFromInt(attrlist.Ints[0], cm.ConfigurationStore)
		id := attrlist.CredentialType()
		var ct CredentialTypeIdentifier
		if id != nil {
			ct = id.Identifier()
		}
		if _, contains := list[ct]; !contains {
			list[ct] = []*AttributeList{}
		}
		list[ct] = append(list[ct], attrlist)
	}

	return list, nil
}

func (cm *CredentialManager) loadKeyshareServers() (ksses map[SchemeManagerIdentifier]*keyshareServer, err error) {
	ksses = make(map[SchemeManagerIdentifier]*keyshareServer)
	exists, err := PathExists(cm.path(kssFile))
	if err != nil || !exists {
		return
	}
	bytes, err := ioutil.ReadFile(cm.path(kssFile))
	if err != nil {
		return nil, err
	}
	err = json.Unmarshal(bytes, &ksses)
	if err != nil {
		return nil, err
	}
	return
}

func (cm *CredentialManager) loadPaillierKeys() (key *paillierPrivateKey, err error) {
	exists, err := PathExists(cm.path(paillierFile))
	if err != nil || !exists {
		return
	}
	bytes, err := ioutil.ReadFile(cm.path(paillierFile))
	if err != nil {
		return nil, err
	}
	key = new(paillierPrivateKey)
	err = json.Unmarshal(bytes, key)
	if err != nil {
		return nil, err
	}
	return
}

func (cm *CredentialManager) loadLogs() (logs []*LogEntry, err error) {
	logs = []*LogEntry{}
	exists, err := PathExists(cm.path(logsFile))
	if err != nil || !exists {
		return
	}
	bytes, err := ioutil.ReadFile(cm.path(logsFile))
	if err != nil {
		return nil, err
	}
	err = json.Unmarshal(bytes, &logs)
	if err != nil {
		return nil, err
	}
	return
}
