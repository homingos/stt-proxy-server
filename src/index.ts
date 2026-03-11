import http from "http";
import WebSocket, { WebSocketServer } from "ws";
import speech, { protos } from "@google-cloud/speech";

const PORT = Number(process.env.PORT) || 8027;

const STREAMING_CONFIG: protos.google.cloud.speech.v1.IStreamingRecognitionConfig = {
    config: {
        encoding: "LINEAR16",
        sampleRateHertz: 16000,
        languageCode: "en-IN",
        model: "latest_long",
        enableAutomaticPunctuation: false,
    },
    interimResults: true,
};

const speechClient = new speech.SpeechClient();

// ─── Helper: create a fresh gRPC recognize stream ──────────────────────────
function createRecognizeStream(ws: WebSocket) {
    return speechClient
        .streamingRecognize(STREAMING_CONFIG)
        .on("data", (data: protos.google.cloud.speech.v1.IStreamingRecognizeResponse) => {
            const result = data.results?.[0];
            if (!result) return;

            const transcript = result.alternatives?.[0]?.transcript ?? "";
            const isFinal = result.isFinal ?? false;

            if (!transcript) return;

            console.log(`[Proxy] Transcript: "${transcript}" (final: ${isFinal})`);

            if (ws.readyState === WebSocket.OPEN) {
                ws.send(JSON.stringify({ transcript, isFinal }));
            }
        })
        .on("error", (err: Error) => {
            console.error("[Proxy] gRPC stream error:", err.message);
            if (ws.readyState === WebSocket.OPEN) {
                ws.send(JSON.stringify({ error: err.message }));
            }
        })
        .on("end", () => {
            console.log("[Proxy] gRPC stream ended");
        });
}

// ─── HTTP & WebSocket Server ────────────────────────────────────────────────
const server = http.createServer((req, res) => {
    if (req.url === "/stt-proxy-server/health" || req.url === "/health") {
        res.writeHead(200, { "Content-Type": "text/plain" });
        res.end("OK");
        return;
    }
    res.writeHead(404);
    res.end();
});

const wss = new WebSocketServer({ server });

server.listen(PORT, () => {
    console.log(`[Proxy] Listening on port ${PORT}`);
});

wss.on("connection", (ws: WebSocket) => {
    console.log("[Proxy] Browser connected");

    let recognizeStream: ReturnType<typeof createRecognizeStream> | null = null;
    let silenceInterval: NodeJS.Timeout | null = null;
    let lastAudioTime = Date.now();

    function getOrCreateStream() {
        if (!recognizeStream || recognizeStream.destroyed) {
            recognizeStream = createRecognizeStream(ws);
        }
        return recognizeStream;
    }

    // Start sending silence to keep stream alive if we haven't received audio recently
    function startSilenceGenerator() {
        if (silenceInterval) return;
        silenceInterval = setInterval(() => {
            if (Date.now() - lastAudioTime >= 2000) {
                // Send 100ms of empty 16-bit PCM audio (16000 Hz = 1600 samples = 3200 bytes)
                const silence = Buffer.alloc(3200, 0);
                if (recognizeStream && recognizeStream.writable && !recognizeStream.destroyed) {
                    recognizeStream.write(silence);
                }
            }
        }, 1000);
    }

    function stopSilenceGenerator() {
        if (silenceInterval) {
            clearInterval(silenceInterval);
            silenceInterval = null;
        }
    }

    // Initialize the stream and silence generator as soon as they connect
    getOrCreateStream();
    startSilenceGenerator();

    ws.on("message", (data: WebSocket.RawData) => {
        // JSON control message (e.g. commit signal)
        if (!Buffer.isBuffer(data)) {
            try {
                const msg = JSON.parse(data.toString());
                if (msg.type === "commit") {
                    console.log("[Proxy] Commit received — flushing gRPC stream");
                    if (recognizeStream && !recognizeStream.destroyed) recognizeStream.end();
                    recognizeStream = null;
                    // We DO NOT stop the silence generator here. The next time the interval
                    // fires (if they haven't started talking), it will call getOrCreateStream()
                    // internally when it tries to write, which spins up a fresh stream immediately
                    // so it's warm when they actually talk.
                    getOrCreateStream(); // eagerly recreate so it's warm
                }
            } catch (_) { }
            return;
        }

        // Binary audio — skip empty keep-alive buffers, but update timestamp
        if (data.length === 0) {
            return;
        }

        lastAudioTime = Date.now();
        const stream = getOrCreateStream();
        if (stream.writable && !stream.destroyed) {
            try {
                stream.write(data);
            } catch (err) {
                console.error("[Proxy] Error writing to stream:", err);
            }
        }
    });

    ws.on("close", (code: number, reason: Buffer) => {
        console.log(`[Proxy] Browser disconnected — code: ${code}, reason: ${reason.toString() || "none"}`);
        stopSilenceGenerator();
        if (recognizeStream && !recognizeStream.destroyed) recognizeStream.end();
    });

    ws.on("error", (err: Error) => {
        console.error("[Proxy] WebSocket error:", err.message);
        stopSilenceGenerator();
        if (recognizeStream && !recognizeStream.destroyed) recognizeStream.end();
    });
});
