async function fetchVideoDuration(name) {
    try {
        const fullPath = buildFullPath(name);

        const resp = await fetch('/api/fs/get', {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ path: fullPath })
        });
        if (!resp.ok) return 0;

        const json = await resp.json();
        const data = json && json.data;
        if (!data) return 0;

        // 1) 优先从 Header 读 X-Video-Duration
        const h = data.header || {};
        const durStr = h['X-Video-Duration'] || h['x-video-duration'];
        if (durStr) {
            const n = parseFloat(durStr);
            if (isFinite(n) && n > 0) return n;
        }

        // 2) 兼容其他字段名（上游若改回 body 字段）
        const candidates = [
            data.video_duration,
            data.duration,
            data.metadata && data.metadata.video_duration,
            data.metadata && data.metadata.duration
        ];
        for (const c of candidates) {
            const n = typeof c === 'string' ? parseFloat(c) : c;
            if (typeof n === 'number' && isFinite(n) && n > 0) {
                return n;
            }
        }
        return 0;
    } catch (e) {
        return 0;
    }
}