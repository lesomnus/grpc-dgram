import { defineConfig } from 'tsdown'

export default defineConfig({
  entry: {
    index: 'src/index.ts',
    'transport/webrtc': 'src/transport/webrtc/index.ts',
    'transport/websocket': 'src/transport/websocket/index.ts',
    'transport/port': 'src/transport/port/index.ts',
    'transport/port/wasm': 'src/transport/port/wasm.ts',
    'transport/node-udp': 'src/transport/node-udp/index.ts',
    'transport/protobuf-es': 'src/transport/protobuf-es/index.ts',
    'transport/connect': 'src/transport/connect/index.ts',
  },
  dts: true,
})
