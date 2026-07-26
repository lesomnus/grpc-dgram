import { defineConfig } from 'tsdown'

export default defineConfig({
  entry: {
    index: 'src/index.ts',
    'transport/webrtc': 'src/transport/webrtc/index.ts',
    'transport/websocket': 'src/transport/websocket/index.ts',
    'transport/port': 'src/transport/port/index.ts',
    wasm: 'src/wasm/index.ts',
    'wasm/worker': 'src/wasm/worker.ts',
    'transport/node-udp': 'src/transport/node-udp/index.ts',
    'transport/protobuf-es': 'src/transport/protobuf-es/index.ts',
    'transport/connect': 'src/transport/connect/index.ts',
  },
  dts: true,
})
