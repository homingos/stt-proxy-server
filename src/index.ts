import http from "http";
import WebSocket, { WebSocketServer } from "ws";
import speech, { protos } from "@google-cloud/speech";

const PORT = Number(process.env.PORT) || 8027;

const STREAMING_CONFIG: protos.google.cloud.speech.v1.IStreamingRecognitionConfig = {
    config: {
        encoding: "LINEAR16",
        sampleRateHertz: 16000,
        model: "latest_long",
        useEnhanced: true,
        languageCode: "en-IN",
        alternativeLanguageCodes: ["en-US", "en-GB", "en-AU"],
        enableAutomaticPunctuation: false,
    },
    interimResults: true,
};

const STREAMING_LIMIT_MS = 240 * 1000;
const OVERLAP_MS = 2000;
const SILENCE_INTERVAL_MS = 200;
const SILENCE_BYTES = 16000 * 2 * (SILENCE_INTERVAL_MS / 1000); // 6400 bytes

const speechClient = new speech.SpeechClient();

function createRecognizeStream(ws: WebSocket) {
    return speechClient
        .streamingRecognize(STREAMING_CONFIG)
        .on("data", (data: protos.google.cloud.speech.v1.IStreamingRecognizeResponse) => {
            const result = data.results?.[0];
            if (!result) return;

            const transcript = result.alternatives?.[0]?.transcript ?? "";
            const isFinal = result.isFinal ?? false;
            const languageCode = result.languageCode ?? "";

            if (!transcript) return;

            console.log(`[Proxy] Transcript: "${transcript}" (final: ${isFinal}, lang: ${languageCode})`);

            if (ws.readyState === WebSocket.OPEN) {
                ws.send(JSON.stringify({ transcript, isFinal, languageCode }));
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

wss.on("connection", (ws: WebSocket, req: http.IncomingMessage) => {
    const ip = req.headers["x-forwarded-for"] || req.socket.remoteAddress || "unknown";
    const userAgent = req.headers["user-agent"] || "unknown";
    const device = /iPhone|iPad|iPod/i.test(userAgent)
        ? "iOS"
        : /Android/i.test(userAgent)
            ? "Android"
            : "Web Browser";

    console.log(`[Proxy] Client connected — device: ${device}, ip: ${ip}, ua: ${userAgent}`);

    let recognizeStream: ReturnType<typeof createRecognizeStream> | null = null;
    let silenceInterval: NodeJS.Timeout | null = null;
    let streamRestartTimeout: NodeJS.Timeout | null = null;
    let lastAudioTime = Date.now();

    function scheduleStreamRestart() {
        if (streamRestartTimeout) clearTimeout(streamRestartTimeout);
        streamRestartTimeout = setTimeout(() => {
            console.log("[Proxy] Proactive stream restart (overlap window)");

            const oldStream = recognizeStream;
            recognizeStream = null;

            // Warm up new stream before killing old
            getOrCreateStream();

            setTimeout(() => {
                if (oldStream && !oldStream.destroyed) {
                    oldStream.end();
                }
            }, OVERLAP_MS);
        }, STREAMING_LIMIT_MS);
    }

    function getOrCreateStream() {
        if (!recognizeStream || recognizeStream.destroyed) {
            recognizeStream = createRecognizeStream(ws);
            scheduleStreamRestart();
        }
        return recognizeStream;
    }

    function startSilenceGenerator() {
        if (silenceInterval) return;
        silenceInterval = setInterval(() => {
            if (Date.now() - lastAudioTime >= 1500) {
                const silence = Buffer.alloc(SILENCE_BYTES, 0);
                if (recognizeStream?.writable && !recognizeStream.destroyed) {
                    recognizeStream.write(silence);
                }
            }
        }, SILENCE_INTERVAL_MS);
    }

    function stopTimers() {
        if (silenceInterval) { clearInterval(silenceInterval); silenceInterval = null; }
        if (streamRestartTimeout) { clearTimeout(streamRestartTimeout); streamRestartTimeout = null; }
    }

    getOrCreateStream();
    startSilenceGenerator();

    ws.on("message", (data: WebSocket.RawData) => {
        if (!Buffer.isBuffer(data)) {
            try {
                const msg = JSON.parse(data.toString());
                if (msg.type === "commit") {
                    console.log("[Proxy] Commit — flushing stream");
                    const oldStream = recognizeStream;
                    recognizeStream = null;
                    if (streamRestartTimeout) { clearTimeout(streamRestartTimeout); streamRestartTimeout = null; }
                    if (oldStream && !oldStream.destroyed) oldStream.end();
                    getOrCreateStream();
                }
            } catch (_) { }
            return;
        }

        if (data.length === 0) return;

        lastAudioTime = Date.now();
        const stream = getOrCreateStream();
        if (stream.writable && !stream.destroyed) {
            try {
                stream.write(data);
            } catch (err) {
                console.error("[Proxy] Write error:", err);
            }
        }
    });

    ws.on("close", (code: number, reason: Buffer) => {
        console.log(`[Proxy] Disconnected — code: ${code}, reason: ${reason.toString() || "none"}`);
        stopTimers();
        if (recognizeStream && !recognizeStream.destroyed) recognizeStream.end();
    });

    ws.on("error", (err: Error) => {
        console.error("[Proxy] WebSocket error:", err.message);
        stopTimers();
        if (recognizeStream && !recognizeStream.destroyed) recognizeStream.end();
    });
});
