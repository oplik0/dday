# D-Day

Strona i system zapisów na unconference **D-Day** w Hakierspejsie w Łodzi.

- **Kiedy:** sobota 8 sierpnia 2026, 14:00–22:00
- **Gdzie:** Hakierspejs, Zielona 30 LU3, Łódź
- **Wstęp:** darmowy, zapisy obowiązkowe (20 miejsc + 20 na liście rezerwowej)
- **Zapisy startują:** niedziela 26 lipca 2026, 15:00 czasu polskiego

Powyższe daty to wartości domyślne — wszystkie są konfigurowalne przez zmienne
środowiskowe, patrz [Zmiana terminu wydarzenia](#zmiana-terminu-wydarzenia).

## Architektura

Dwie binarki Go (bez CGO), wspólny pakiet z datami i konfiguracją terminu:

| Komponent | Ścieżka | Opis |
|---|---|---|
| Serwer WWW | `.` (`main.go`, `server.go`, `templates.go`) | landing + rejestracja; `index.html`, `privacy.html` i `style.css` są `go:embed`-owane w binarce |
| Bot Matrix | `cmd/bot`, `internal/matrixbot` | nasłuchuje `!start`, wysyła DM z prywatnym linkiem (rejestracja albo panel) |
| Bramka czasowa i daty | `internal/regwindow` | jedyne źródło prawdy dla terminów; generuje polskie opisy dat |
| Baza | `internal/store` | SQLite przez `modernc.org/sqlite` (czysty Go) |
| Style | `style.css` | jedyny arkusz stylów; sekcje `body.landing` / `body.page` / `body.privacy` |

Trasy serwera WWW:

| Trasa | Opis |
|---|---|
| `GET /` | landing (`index.html`); tylko dokładnie `/`, żadnego przeglądania katalogu |
| `GET /register?t=…` | formularz zapisu dla ważnego tokenu |
| `POST /register` | zapis (miejscowość + e-mail); tożsamość wyłącznie z tokenu |
| `GET /api/count` | JSON: obłożenie miejsc + wszystkie daty i ich opisy (patrz niżej) |
| `GET /api/registered?h=@user:hs` | wewnętrzne, dla bota; wymaga `Authorization: Bearer $INTERNAL_TOKEN`, bez tokenu 404 |
| `GET /api/registrations?since=<id>` | wewnętrzne, dla bota: zgłoszenia o `id > since` do ogłoszeń na kanale; wymaga `Authorization: Bearer $INTERNAL_TOKEN`, bez tokenu 404, **bez e-maila i miejscowości** |
| `GET /panel?t=…` | panel uczestnika dla ważnego magic linku: status + przycisk „Wycofaj udział" |
| `POST /panel` | wycofanie udziału (tożsamość wyłącznie z tokenu) |
| `GET /admin?t=…` | panel admina: podgląd wszystkich zgłoszeń; wymaga `ADMIN_TOKEN`, bez tokenu 404 |
| `GET /privacy` | polityka prywatności (`privacy.html`) |
| `GET /style.css` | wspólny arkusz stylów wszystkich stron (`style.css`) |
| `GET /healthz` | health check (używany też przez `dday -healthcheck`) |

## Jak działa rejestracja

1. Użytkownik pisze `!start` do bota (DM albo pokój z allowlisty). `!register` działa
   nadal jako alias — komendą oficjalną (tą z landing page) jest `!start`.
2. Bot sprawdza bramkę czasową i — jeśli ma `INTERNAL_TOKEN` — pyta
   `/api/registered`, czy ten handle już się zapisał.
3. Bot generuje token: `{handle, issued}` zaszyfrowane kluczem age (odbiorcą jest
   klucz publiczny serwera), podpisane HMAC-em (`TOKEN_SECRET`), całość base64.
4. Bot wysyła w DM link `REGISTER_URL?t=<token>`.
5. `GET /register?t=…` odszyfrowuje i weryfikuje token: zły podpis → „Nieprawidłowy
   link", starszy niż **48 h** (lub wystawiony w przyszłości ponad 5 min zapasu na
   zegar) → „Link wygasł".
6. Formularz zbiera miejscowość i e-mail; `POST` zapisuje rekord w SQLite i
   przydziela numer uczestnika (`AUTOINCREMENT`).
7. Numery 1–20 to miejsca potwierdzone, 21–40 lista rezerwowa, powyżej — brak miejsc.
8. Link jest jednorazowy: po zapisie ten sam link pokazuje potwierdzenie, a próba
   ponownego `POST` kończy się stroną „Już zapisany", bez drugiego rekordu.

## Panel uczestnika

Kolejne `!start` od kogoś, kto jest już zapisany (`/api/registered`), nie jest odmową —
bot wysyła w DM **magic link do panelu** (`PANEL_URL?t=<token>`), a na kanale zostawia
tylko publiczny nudge „sprawdź prywatne wiadomości" (numeru uczestnika nie ujawnia publicznie).

- Token panelu ma ten sam mechanizm co rejestracyjny (age + HMAC, **TTL 48 h**), ale niesie
  inny *kind* (`panel` vs `reg`), objęty HMAC-em. Dzięki temu link rejestracyjny nie otworzy
  panelu, a magic link do panelu nie pozwoli się zapisać.
- W panelu widać nick, numer uczestnika i status (uczestnik / lista rezerwowa, pozycja).
  Jedyną akcją jest **„Wycofaj udział"** — usuwa rekord i zwalnia miejsce (osoba z rezerwy awansuje).
- Po wycofaniu można zapisać się ponownie: `!start` → bot widzi, że nie jesteś zapisany →
  link rejestracyjny (o ile są miejsca).
- Gdy `PANEL_URL` nie da się ustalić, bot degraduje się do dawnej publicznej odpowiedzi
  „jesteś już zapisany", zamiast wysyłać zepsuty link.

## Panel admina

`GET /admin` to **widok tylko do odczytu** dla organizatora: podsumowanie obłożenia
(uczestnicy `X/20`, lista rezerwowa `Y/20`, łącznie) oraz tabela wszystkich zgłoszeń —
numer uczestnika, status (`uczestnik` / `rezerwa #poz`), nick, handle Matrix,
miejscowość, e-mail i data zapisu (strefa Europe/Warsaw).

Dostęp chroni `ADMIN_TOKEN`. Token można podać na dwa sposoby:

```
https://dday.hs-ldz.pl/admin?t=<ADMIN_TOKEN>
curl -H "Authorization: Bearer <ADMIN_TOKEN>" https://dday.hs-ldz.pl/admin
```

- Token bierze się z pliku `.env` — `make up` generuje go raz i potem reużywa
  (`grep ADMIN_TOKEN .env`).
- Pusty `ADMIN_TOKEN` **wyłącza endpoint całkowicie** (404), tak samo jak
  `INTERNAL_TOKEN` wyłącza `/api/registered`. Zły lub brakujący token → 401.
  Porównanie jest constant-time.
- **URL zawiera sekret** — nie wklejaj go na czat, do issue ani w screenshot.
  Odpowiedź ma `Cache-Control: no-store`, a globalne `Referrer-Policy: no-referrer`
  pilnuje, żeby token nie wyciekł nagłówkiem Referer. Kto ma token, ten widzi
  dane osobowe wszystkich zapisanych.
- Panel **nic nie modyfikuje**: wycofanie udziału robi sam uczestnik w `/panel`,
  a awaryjne usunięcie danych to CLI `dday -delete <handle>` (patrz sekcja RODO).
- Status liczony jest z **rangi** (pozycji wśród aktualnych wierszy), więc po
  wycofaniu się uczestnika awans z listy rezerwowej widać od razu.

## Ogłoszenia nowych zapisów

Rejestracja kończy się po stronie WWW, więc bot nie dowiaduje się o niej sam — **cyklicznie
odpytuje** wewnętrzny endpoint `GET /api/registrations?since=<ostatnie ogłoszone id>`
(bearer `INTERNAL_TOKEN`) i ogłasza to, czego jeszcze nie ogłosił, na kanale macierzystym:

```
🎉 alice dołącza do D-Day — uczestnik #5
🎉 bob zapisał(a) się na D-Day — lista rezerwowa, pozycja #2
```

- **Bez danych osobowych.** Ogłoszenie zawiera wyłącznie handle Matrix (jako mention
  przez `matrix.to`) i numer uczestnika albo pozycję na liście rezerwowej. E-mail
  i miejscowość nie wychodzą poza serwer — endpoint `/api/registrations` w ogóle
  ich nie zwraca, a typ, którym posługuje się bot, nie ma na nie pól.
- **Bez powtórek po restarcie.** Ostatnie ogłoszone `id` jest zapisywane w tym samym
  pliku co cache DM (`DM_CACHE_PATH`, format `{"dms":{…},"lastAnnounced":N}`; stary,
  płaski format wczytuje się nadal, z `lastAnnounced=0`).
- **Pierwszy start bez stanu nie odtwarza historii**: bot ustawia `lastAnnounced` na
  aktualne maksymalne `id` i ogłasza dopiero kolejne zapisy.
- Wyłączenie: puste `ANNOUNCE_ROOM` (i `MATRIX_ROOM`) albo brak `INTERNAL_TOKEN`.

Landing pobiera stan z `/api/count` (licznik zapisanych, pasek, lista rezerwowa,
flaga `open` i daty). Bez API (np. GitHub Pages) strona degraduje się do wartości
wpisanych na sztywno w `index.html`.

## Konfiguracja

Serwer WWW:

| Zmienna | Domyślnie | Opis |
|---|---|---|
| `PORT` | `3329` | port nasłuchu |
| `STATIC_DIR` | *(puste)* | katalog z `index.html`/`privacy.html`/`style.css` zamiast wersji wbudowanej (dev) |
| `DB_PATH` | `./dday.db` | plik bazy SQLite |
| `AGE_KEY` | `config/dday_ed25519` | ścieżka klucza prywatnego age |
| `AGE_KEY_DATA` | *(puste)* | ten sam klucz przekazany base64 (ma pierwszeństwo; używane w kontenerze) |
| `TOKEN_SECRET` | *(puste)* | wspólny sekret HMAC dla tokenów; musi być identyczny u bota |
| `INTERNAL_TOKEN` | *(puste)* | bearer chroniący `/api/registered` i `/api/registrations`; puste = endpointy wyłączone (404) |
| `ADMIN_TOKEN` | *(puste)* | token chroniący `/admin`; puste = panel admina wyłączony (404) |
| `REGISTRATION_OPEN` | *(puste)* | **opcjonalny override do testów**: `1`/`true`/`yes` wymusza otwarte zapisy, `0` wymusza zamknięte, brak/puste = automat wg `REGISTRATION_OPEN_AT` |
| `REGISTRATION_OPEN_AT` | `2026-07-26 15:00` | moment otwarcia zapisów |
| `EVENT_START_AT` | `2026-08-08 14:00` | początek wydarzenia |
| `EVENT_END_AT` | `2026-08-08 22:00` | koniec wydarzenia |

Bot Matrix (`cmd/bot`, konfiguracja zwykle w `matrix.env`):

| Zmienna | Domyślnie | Opis |
|---|---|---|
| `MATRIX_HOMESERVER` | **wymagane** | np. `https://matrix.org` |
| `MATRIX_USER` | **wymagane** | np. `@ddaybot:matrix.org` |
| `MATRIX_PASSWORD` | **wymagane** | hasło bota |
| `MATRIX_ROOM` | *(puste)* | pokój używany przez skrypty `make matrix-hello` / `matrix-send` |
| `REGISTER_URL` | `https://dday.hs-ldz.pl/` | baza linku rejestracyjnego (jej origin służy też do `/api/registered`) |
| `PANEL_URL` | `origin(REGISTER_URL) + /panel` | baza magic linku do panelu uczestnika |
| `AGE_PUB` | `config/dday_ed25519.pub` | klucz publiczny age, którym bot szyfruje tokeny |
| `AGE_PUB_DATA` | *(puste)* | ten sam klucz base64 (ma pierwszeństwo) |
| `TOKEN_SECRET` | *(puste)* | jak wyżej — musi zgadzać się z serwerem |
| `INTERNAL_TOKEN` | *(puste)* | włącza pytanie `/api/registered` przed wydaniem linku oraz ogłaszanie nowych zapisów (`/api/registrations`) |
| `ANNOUNCE_ROOM` | `MATRIX_ROOM` | pokój, w którym bot ogłasza nowe zapisy; puste = ogłaszanie wyłączone |
| `ANNOUNCE_INTERVAL` | `30s` | jak często bot odpytuje `/api/registrations` (format `time.ParseDuration`) |
| `ALLOWED_ROOMS` | *(puste)* | lista room id po przecinku; puste = bot reaguje wszędzie |
| `DM_CACHE_PATH` | *(puste)* | plik stanu bota (cache `handle → room id` + `lastAnnounced`), przeżywa restart |
| `REGISTRATION_OPEN`, `REGISTRATION_OPEN_AT` | jak wyżej | bot używa tej samej bramki i tego samego opisu daty |

Formaty dat (`*_AT`): RFC3339 (`2026-07-26T15:00:00+02:00`) albo
`2006-01-02 15:04` / `2006-01-02 15:04:05` / `2006-01-02` interpretowane w strefie
Europe/Warsaw. Wartość niepoprawna → wpis w logu i użycie domyślnej (bez crasha).

## Uruchomienie lokalnie

```sh
make keys       # jednorazowo: para kluczy age do config/ (gitignored)
make run        # serwer WWW na :3329 (index.html z binarki)
make dev        # to samo, ale index.html czytany z dysku (live edit)
make bot        # bot Matrix, konfiguracja z ./matrix.env
make check      # walidacja matrix.env
make test       # go test -race ./...
```

Sekrety trzymamy poza repo: `matrix.env` (wzór w `matrix.env.example`), `.env`
generowany przez `make up` i katalog `config/` — wszystkie są w `.gitignore`.

Podgląd z niestandardowymi datami:

```sh
REGISTRATION_OPEN_AT="2026-09-01 12:00" \
EVENT_START_AT="2026-09-05 10:00" EVENT_END_AT="2026-09-05 18:00" make run
```

## Deployment

```sh
make up     # generuje .env i odpala docker compose up -d --build
make logs   # docker compose logs -f
make down   # zatrzymanie stacku
```

`make up` sprawdza obecność kluczy age i `matrix.env`, a następnie zapisuje `.env`
z: `AGE_KEY_DATA`, `AGE_PUB_DATA` (klucze base64 — plik 0600 byłby nieczytelny dla
użytkownika `nonroot` w obrazie distroless), `INTERNAL_TOKEN`, `TOKEN_SECRET` i
`ADMIN_TOKEN` (losowane raz i potem reużywane) oraz `REGISTRATION_OPEN`
(domyślnie **pusty** — czyli rejestracja otwiera się automatycznie, patrz niżej;
jawnie ustawiona wartość z istniejącego `.env` jest zachowywana). Po zapisie
`make up` wypisuje tryb rejestracji: `AUTO`, `FORCED OPEN` albo `FORCED CLOSED`.

`docker-compose.yml` wystawia serwis `dday` przez Traefika na
`dday.hs-ldz.pl` (entrypoint `websecure`, certresolver `myresolver`) i uruchamia
serwis `bot`. Dane trwałe: wolumen `dday-data` (`/data/dday.db`) i `bot-data`
(cache DM-ów bota). Zmienne `REGISTRATION_OPEN_AT` / `EVENT_START_AT` /
`EVENT_END_AT` są przepuszczane do obu serwisów — ustaw je w `.env`, jeśli
zmieniasz termin.

## Automatyczne otwarcie zapisów

**Nie trzeba nic robić ręcznie w momencie startu zapisów.** Rejestracja otwiera
się sama o `REGISTRATION_OPEN_AT` (domyślnie `2026-07-26 15:00` czasu polskiego):

- **Serwer WWW** liczy bramkę `regwindow.Open()` przy **każdym żądaniu**, więc
  `/register` i `/api/count` przełączają się w tej samej sekundzie — bez
  restartu i bez redeploya.
- **Bot Matrix** liczy ją przy **każdej komendzie**, więc od tego momentu zaczyna
  wydawać linki zamiast odpowiadać „jeszcze nie wystartowały".
- **Landing** odpytuje `/api/count` co 60 s i dodatkowo natychmiast w chwili, gdy
  odliczanie do startu przekroczy zero. Otwarta w przeglądarce strona sama
  pokaże licznik zapisanych, sekcję „Jak się zapisać" i aktywne CTA — **bez F5**.
  Jeśli API kiedykolwiek odpowiedziało, to ono rozstrzyga (uwzględnia też
  wymuszone `REGISTRATION_OPEN=0`); bez API (np. GitHub Pages) decyduje zegar
  przeglądarki i wbudowana data fallbackowa.

`REGISTRATION_OPEN` to **opcjonalny override wyłącznie do testów**: `1` wymusza
otwarte zapisy, `0` wymusza zamknięte, brak/puste = automat.

### Uwaga dla istniejących wdrożeń

Starsze wersje `make up` wpisywały do `.env` `REGISTRATION_OPEN=1`. Taka linia
**wymusza otwarte zapisy na stałe i całkowicie ignoruje moment otwarcia**. Na
hoście produkcyjnym trzeba ją usunąć (albo wyzerować) przed startem zapisów:

```sh
cd /ścieżka/do/dday
grep '^REGISTRATION_OPEN=' .env          # sprawdź, co tam jest
sed -i 's/^REGISTRATION_OPEN=.*/REGISTRATION_OPEN=/' .env   # pusta wartość = automat
docker compose up -d                     # przeładuj env kontenerów
curl -s https://dday.hs-ldz.pl/api/count | grep -o '"open":[a-z]*'
```

Ostatnia komenda przed terminem musi pokazać `"open":false`. Alternatywnie
`make up` — zachowa jawną wartość, jeśli linia dalej ma `1`, więc i tak trzeba ją
najpierw wyczyścić. Restart nie jest potrzebny do samego otwarcia — jedynie do
wczytania zmienionego `.env`.

## Zmiana terminu wydarzenia

Terminy pochodzą wyłącznie z `internal/regwindow`: serwer używa ich na stronach
„zapisy nieotwarte", bot w odpowiedzi „jeszcze nie wystartowały", a landing
pobiera je z `/api/count` (`openAt`, `eventStartAt`, `eventEndAt` + gotowe polskie
teksty `openText`, `openHowto`, `openShort`, `openShortTime`, `eventText`,
`eventShort`, `eventShortTime`, `eventBadge`). Polskie odmiany („niedziela”,
„lipca”, „w niedzielę”) generuje sam pakiet — nic nie synchronizuje się ręcznie.

Żeby przesunąć termin, ustaw w `.env` (deployment) albo w środowisku procesu:

```sh
REGISTRATION_OPEN_AT="2026-09-01 12:00"
EVENT_START_AT="2026-09-05 10:00"
EVENT_END_AT="2026-09-05 18:00"
```

Restart kontenerów i tyle — landing, formularz i bot mówią to samo.

Ręcznie trzeba poprawić tylko rzeczy, których nie da się wyliczyć w runtime:

- `index.html`: `<meta name="description">` i `<meta property="og:description">` —
  statyczne, bo crawlery nie wykonują naszego JS.
- `index.html`: literały fallbackowe (`REG_OPEN`, `EVENT`, teksty kafelków) —
  używane wyłącznie, gdy strona jest serwowana bez API (np. GitHub Pages). Warto
  je odświeżyć przy zmianie terminu, choć produkcja bierze daty z API.
- Nagłówek tego README.
- `privacy.html` — pole „Ostatnia aktualizacja”, jeśli zmieniacie samą politykę.

## RODO — usuwanie danych uczestnika

Na żądanie uczestnika (kontakt: kontakt@hakierspejs.pl) usuwamy jego rekord
z bazy zapisów. Służy do tego wbudowany tryb CLI binarki, wskazujący uczestnika
po identyfikatorze Matrix (`matrix_handle`):

```sh
# w kontenerze (usługa dday używa /data/dday.db z wolumenu):
docker compose run --rm dday -delete @user:homeserver

# lub bezpośrednio przy uruchomionej binarce (DB_PATH wskazuje bazę):
DB_PATH=./dday.db dday -delete @user:homeserver
```

Komenda wypisuje wynik i kończy się kodem `0`, gdy rekord został usunięty, albo
`1`, gdy takiego zapisu nie było (lub wystąpił błąd). Uwaga: numer uczestnika to
`AUTOINCREMENT` — po usunięciu **nie jest odtwarzany** i nie zostaje przydzielony
ponownie. To celowe i akceptowalne.

## Testy

```sh
make test          # go test -race -tags goolm ./...
make fmt vet       # gofmt -w . && go vet -tags goolm ./...
```

Pokrycie: pełny przepływ rejestracji (token → formularz → zapis → duplikat →
lista rezerwowa → komplet), TTL i manipulacja tokenem, bramka czasowa i parsowanie
`*_AT`, generowanie polskich opisów dat, pola `/api/count`, nagłówki
bezpieczeństwa, tryb `-delete`, ładowanie kluczy oraz logika bota (allowlista
pokojów, cache DM-ów, odpowiedzi przed otwarciem zapisów).

Zwykłe `make test` używa wewnętrznego, in-memory fake'a transportu Matrix i
**nie potrzebuje serwera** — jest hermetyczne i szybkie.

### Test E2EE (prawdziwy Synapse)

`make e2e` uruchamia integracyjny test szyfrowanego przepływu przeciw
**prawdziwemu Synapse** w Dockerze (potrzebny Docker):

```sh
make e2e           # stawia Synapse, rejestruje userów bot+alice, odpala test, sprząta
```

Test (`test/e2e/e2e_test.go`, za build-tagami `e2e && goolm`) prowadzi
**prawdziwego bota** (`matrixbot.NewMxClient` + `Client.Serve`, ten sam
konstruktor co `cmd/bot`) i drugiego klienta „alice”: alice tworzy **szyfrowany**
pokój, zaprasza bota, wysyła zaszyfrowane `!start`; bot je deszyfruje, buduje link
rejestracyjny i odpowiada **zaszyfrowaną** wiadomością, którą alice deszyfruje —
a token z linku jest dekodowany i weryfikowany (`Handle == @alice`, `Kind == reg`).
Drugi przypadek powtarza to w pokoju **nieszyfrowanym** (brak regresji plaintext).
Ręcznie, przeciw działającemu serwerowi:

```sh
bash scripts/e2e-synapse.sh up
go test -tags 'e2e goolm' -run E2E -v ./test/e2e/...
bash scripts/e2e-synapse.sh down
```

Bez serwera test **SKIP-uje** (nie failuje), więc nie blokuje deweloperów bez Dockera.

CI (GitHub Actions, `.github/workflows`) uruchamia `gofmt -l`, `go vet`,
`go build`, `go test -race` oraz build obrazu Dockera jako **wymagane** joby.
Test E2EE z Synapse jest osobnym, **opcjonalnym** jobem `e2e`
(`continue-on-error: true`, także `workflow_dispatch`) — bywa cięższy/flaky, więc
nigdy nie blokuje merge'a; źródłem prawdy jest lokalne `make e2e`.
