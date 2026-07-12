import { defineConfig } from 'tsdown'

export default defineConfig({
  entry: {
    index: 'src/index.ts',
    'transport/webrtc': 'src/transport/webrtc/index.ts',
    'transport/node-udp': 'src/transport/node-udp/index.ts',
    'transport/protobuf-es': 'src/transport/protobuf-es/index.ts',
    'transport/connect': 'src/transport/connect/index.ts',
  },
  dts: true,
})
