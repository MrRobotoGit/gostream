# Piano: Fix Micro-Blocco alla Transizione Warmup → raCache

**Branch di lavoro**: `claude/unify-native-bridge-8R0xs`
**Basato su**: `origin/main` @ `8c21b8e` (merge già eseguito)
**File modificato**: `main.go` (solo 3 punti chirurgici)

---

## Root Cause

All'avvio del playback con cache SSD disponibile (`hasWarmup=true`), si verifica un micro-blocco
esattamente a `warmupFileSize` (default 64MB). Il flusso che lo causa:

```
Open()          → pumpOff=-1, pump lazy (V310)
Read(off=0)     → pumpOnce.Do: pumpOff=0, startNativePump()
                → SSD serve 0..64MB velocemente (diskWarmup.ReadAt)
startNativePump → resumeOffset = diskOffset - 32MB (safety margin V294) = 32MB
                → V303 skip: 32MB→48MB→64MB (pump salta la zona warmup)
                → Prima read da torrent a warmupFileSize (64MB)

Read(off=64MB)  → diskWarmup.ReadAt() = 0 byte (fuori zona)
                → raCache.CopyTo() = MISS (pump non ha ancora riempito 64MB)
                → FetchBlock bloccante (2-8s) ← MICRO-BLOCCO
```

**Causa principale**: race tra player (veloce su SSD) e pump goroutine (deve partire, skipkare
V303, e aspettare anacrolix per i piece a 64MB).

**Fattori aggravanti**:
- Goroutine scheduling overhead al lancio del pump
- `resumeOffset = 64MB - 32MB = 32MB` invece di 64MB direttamente
- `pumpOff=-1` in Open() → non viene reimpostato a 0 prima del pump start

---

## Fix 1 — Early pump start in `Open()` quando `hasWarmup=true`

**Dove**: `main.go`, funzione `VirtualMkvNode.Open()`, dopo il blocco `if isNative { ... }`

**Problema**: il pump parte al primo `Read(off=0)`. Ma la SSD è così veloce che il player
percorre 64MB prima che il pump goroutine venga schedulato, skippi V303 e faccia la prima
read da anacrolix.

**Fix**: avviare il pump subito in `Open()` quando `hasWarmup=true`, impostando `pumpOff=0`
(il player partirà da 0, non da -1). Il pump avrà tutto il tempo in cui il player legge 64MB
da SSD per portarsi a `warmupFileSize` e scaricare il primo chunk da anacrolix.

```go
// Alla fine di Open(), subito prima di activeHandles.Store(h, true):
if isNative && hasWarmup {
    h.pumpOff = 0 // player partirà da 0
    go h.pumpOnce.Do(func() {
        h.startNativePump(finalHash, fileIdx)
    })
}

// V182: Register for cleanup
activeHandles.Store(h, true)
```

**Impatto**: il pump ha ~1-2 secondi di vantaggio rispetto al player.
**Rischio**: nessuno — `pumpOnce` garantisce che la seconda chiamata da `Read()` sia no-op.

---

## Fix 2 — Pump landing diretto a `warmupFileSize` in `startNativePump`

**Dove**: `main.go`, funzione `startNativePump`, nel blocco `// V261: Strategic pump skip`

**Problema**: con `diskOffset=64MB` e `safetyMargin=32MB`, il pump parte a 32MB. V303 fa
2 iterazioni di skip (32MB→48MB→64MB) prima di iniziare la read reale. Piccolo overhead ma
evitabile quando `hasWarmup=true` e il warmup è completo.

**Fix**: quando `h.hasWarmup && diskOffset >= warmupFileSize`, impostare `resumeOffset =
warmupFileSize` direttamente, bypassing il calcolo `diskOffset - safetyMargin`.

```go
// Nel blocco "V261: Strategic pump skip", dopo la validazione header:
if diskOffset > 32*1024*1024 {
    if h.hasWarmup && diskOffset >= warmupFileSize {
        // Warm start completo: atterrare esattamente alla frontiera (no V303 skip inutili)
        if warmupFileSize > resumeOffset {
            resumeOffset = warmupFileSize
            logger.Printf("[DiskWarmup] PUMP DIRECT: Starting at %.1fMB (warmup boundary)",
                float64(warmupFileSize)/(1<<20))
        }
    } else {
        safetyMargin := int64(32 * 1024 * 1024) // V294: 32MB per 4K
        skipOffset := diskOffset - safetyMargin
        if skipOffset > resumeOffset {
            resumeOffset = skipOffset
            logger.Printf("[DiskWarmup] PUMP SKIP: Starting from %.1fMB (Disk: %.1fMB, Margin: 32MB)",
                float64(resumeOffset)/(1<<20), float64(diskOffset)/(1<<20))
        }
    }
}
```

**Impatto**: il pump atterra a `warmupFileSize` in un solo step senza iterazioni V303.
**Nota**: la validazione header esistente rimane invariata sopra questo blocco.

---

## Fix 3 — Pre-fetch boundary in `Read()` SSD warmup path

**Dove**: `main.go`, funzione `MkvHandle.Read()`, nel blocco `// V256: Disk warmup cache`

**Problema**: anche con Fix 1+2, esiste un edge case in cui il player è già vicino a
`warmupFileSize` quando il pump non ha ancora scaricato il chunk di transizione (es. ripresa
da seek vicino alla frontiera, o SSD NVMe molto veloce).

**Fix**: quando si sta servendo da SSD warmup e l'offset è nell'ultimo chunk prima della
frontiera, lanciare un FetchBlock asincrono per pre-popolare `raCache[warmupFileSize]`.
Il FetchBlock usa il percorso diretto al reader (già ottimizzato in `b3abd68`).

```go
// Nel blocco SSD warmup, dopo "n > 0" e prima del return:
if diskWarmup != nil && h.hash != "" && off < warmupFileSize {
    n, _ := diskWarmup.ReadAt(h.hash, h.fileID, dest, off)
    if n > 0 {
        timing.UsedCache = true
        timing.BytesRead = n
        if off == 0 {
            logger.Printf("[DiskWarmup] HIT %s off=0 (%dKB)", filepath.Base(h.path), n/1024)
        }

        // Fix 3: Approaching boundary → pre-fetch chunk di transizione in background
        chunkSize := int64(globalConfig.ReadAheadBase)
        if chunkSize == 0 {
            chunkSize = 16 * 1024 * 1024
        }
        nearBoundary := off+int64(n) >= warmupFileSize-chunkSize && off+int64(n) <= warmupFileSize
        if nearBoundary && !raCache.Exists(h.path, warmupFileSize) {
            prefetchKey := fmt.Sprintf("%s:wb", h.path)
            if _, loaded := inFlightPrefetches.LoadOrStore(prefetchKey, true); !loaded {
                goHash, goFileID, goPath, goSize := h.hash, h.fileID, h.path, h.size
                safeGo(func() {
                    defer inFlightPrefetches.Delete(prefetchKey)
                    select {
                    case masterDataSemaphore <- struct{}{}:
                        defer func() { <-masterDataSemaphore }()
                    case <-time.After(200 * time.Millisecond):
                        return
                    }
                    bufPtr := readBufferPool.Get().(*[]byte)
                    defer readBufferPool.Put(bufPtr)
                    limit := int64(len(*bufPtr))
                    if goSize-warmupFileSize < limit {
                        limit = goSize - warmupFileSize
                    }
                    if limit <= 0 {
                        return
                    }
                    nf, err := nativeBridge.FetchBlock(goHash, goFileID, warmupFileSize, (*bufPtr)[:limit])
                    if err == nil && nf > 0 {
                        raCache.Put(goPath, warmupFileSize, warmupFileSize+int64(nf)-1, (*bufPtr)[:nf])
                        logger.Printf("[WarmupBoundary] Pre-fetched %.1fKB at %.1fMB",
                            float64(nf)/1024, float64(warmupFileSize)/(1<<20))
                    }
                })
            }
        }

        // V570: Restore lastOff for Head Warmup to keep pump in sync during start.
        atomic.StoreInt64(&h.lastOff, off)

        h.mu.Lock()
        h.lastLen = n
        h.lastTime = now
        h.mu.Unlock()
        return fuse.ReadResultData(dest[:n]), 0
    }
}
```

**Impatto**: safety net che garantisce hit anche in condizioni avverse (SSD NVMe, seek near boundary).
**Costo**: 1 semaphore slot per max 200ms — solo una volta per sessione (prefetchKey deduplicato).

---

## Mappa dei file e righe (codice attuale post-merge)

| Fix | Funzione | Riga indicativa |
|-----|----------|-----------------|
| Fix 1 | `VirtualMkvNode.Open()` | ~riga 922 (dopo `h.fileID = fileIdx`) |
| Fix 2 | `startNativePump()` | ~riga 1080 (blocco `if diskOffset > 32*1024*1024`) |
| Fix 3 | `MkvHandle.Read()` | ~riga 1503 (blocco `if diskWarmup != nil && off < warmupFileSize`) |

---

## Impatto Atteso

| Scenario | Prima | Dopo |
|----------|-------|------|
| Transizione warmup→raCache | FetchBlock bloccante (2-8s) | raCache HIT (pre-fetchato) |
| Pump start timing | First Read() → goroutine overhead | Open() → pump già in moto |
| Prima read da torrent | `diskOffset - 32MB` + 2 skip V303 | `warmupFileSize` diretto (0 skip) |
| Edge case SSD veloce | Micro-blocco possibile | Fix 3 garantisce hit |

---

## Verifica

1. **Build**: `go build ./...` — nessun errore
2. **Log attesi** al primo play con warmup:
   ```
   [DiskWarmup] PUMP DIRECT: Starting at 64.0MB (warmup boundary)
   [WarmupBoundary] Pre-fetched NKB at 64.0MB
   ```
   Assenza di: `[FetchBlock timeout]` durante la transizione
3. **Smoke test**: aprire MKV con warmup disponibile, verificare assenza di pausa a ~64MB
4. **Nessuna regressione**: playback senza warmup, seek rapidi, file multipli simultanei
