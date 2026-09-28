# Ponte

Un mouse e una tastiera per due computer. Porti il puntatore oltre il bordo dello
schermo e continui a lavorare sull'altro computer, come con Synergy o Barrier,
ma in un unico file da circa 6 MB, senza installazione.

## Come si usa

1. Avvia `Ponte` su entrambi i computer (collegati alla stessa rete).
   Si apre una finestra.
2. Sul computer che ha mouse e tastiera scegli **Questo computer ha mouse e tastiera**.
   Compare un codice di 6 cifre.
3. Sull'altro scegli **Questo computer verrà controllato**, clicca sul computer
   trovato in rete e inserisci il codice.
4. Fatto. Porta il puntatore oltre il bordo dello schermo (di default quello
   destro, puoi cambiarlo nella finestra) e passa all'altro computer. Per tornare
   indietro muovi il puntatore dal lato opposto, oppure premi **Bloc Scorr**
   o **Ctrl + Alt + Esc** (utile se la tastiera non ha Bloc Scorr).

Il testo che copi su un computer si incolla anche sull'altro (si può spegnere dal menu ⋮).

L'abbinamento si fa una volta sola: dalle volte successive i due computer si
ricollegano da soli. Dal menu ⋮ puoi attivare **Avvia con il computer**.

Chiudendo la finestra, Ponte resta attivo nell'area di notifica (l'icona del mouse
vicino all'orologio): un clic la riapre, il tasto destro offre **Esci da Ponte**.
Se chiudi Ponte sul computer controllato mentre lo stai usando, il mouse e la
tastiera tornano subito all'altro computer; se l'altro computer smette di
rispondere, tornano indietro da soli entro 3 secondi.

## Primo avvio

**Windows**: essendo un programma nuovo e non firmato, Windows può mostrare
«Windows ha protetto il PC»: clicca *Ulteriori informazioni* → *Esegui comunque*.
Al primo avvio Windows chiede anche di consentire l'accesso alla rete: scegli
**Reti private**.

La finestra usa il componente WebView2, già presente in Windows 10 e 11.

**Linux**: Ponte deve poter leggere e simulare mouse e tastiera. Se manca il
permesso, la finestra lo segnala e il pulsante **Risolvi** lo sistema (chiede la
password di amministratore). Il passaggio al bordo dello schermo richiede una
sessione X11; su Wayland si passa da un computer all'altro con **Bloc Scorr**.

## Sicurezza

Il collegamento è cifrato (X25519 + AES-GCM): chi è nella stessa rete non può
leggere quello che digiti. Solo un computer che conosce il codice può abbinarsi;
dopo 5 codici sbagliati il codice cambia. L'abbinamento usa poi una chiave
casuale salvata sui due computer, che puoi revocare con l'icona del cestino.

## Compilare

Serve Go 1.24 o successivo.

```sh
./build.sh          # crea dist/Ponte.exe (Windows) e dist/ponte-linux-*
go test ./...       # prova end-to-end di abbinamento, bordo, tastiera
```

## Com'è fatto

| File | Cosa contiene |
|---|---|
| `share.go` | computer che condivide: bordo dello schermo, passaggio, Bloc Scorr |
| `recv.go` | computer controllato: riconnessione, riproduzione dell'input |
| `crypto.go`, `proto.go` | collegamento cifrato e messaggi |
| `clip.go`, `clip_windows.go`, `clip_unix.go` | testo copiato condiviso tra i due computer |
| `discovery.go` | annunci in rete (UDP 24802) per trovare l'altro computer |
| `input_windows.go` | hook di basso livello e `SendInput` |
| `input_linux.go` | `/dev/input` (evdev) e `/dev/uinput`, posizione del puntatore da X11 |
| `ui.go`, `web/index.html` | interfaccia (pagina locale su 127.0.0.1:24801) |
| `gui_windows.go` | finestra di Ponte (componente WebView2 di Windows, senza aprire il browser) e icona nell'area di notifica |
| `assets/`, `rsrc_windows_*.syso` | icona del mouse, incorporata nell'eseguibile |

Porte usate: TCP 24800 (collegamento), UDP 24802 (ricerca), TCP 24801 solo locale (finestra).
I tasti viaggiano come tasti fisici: usa lo stesso layout di tastiera (es. italiano) su entrambi i computer.

## Limiti attuali

- macOS non ancora supportato.
- Gli appunti condividono solo testo (non immagini o file). Su Linux serve `xclip` (X11) o `wl-clipboard` (Wayland).
- Su Windows, mentre controlli l'altro computer, il puntatore resta fermo al centro dello schermo locale.
- Le finestre avviate come amministratore su Windows ricevono input da Ponte solo se anche Ponte è avviato come amministratore.
