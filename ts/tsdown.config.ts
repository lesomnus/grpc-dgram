import { defineConfig } from 'tsdown'

export default defineConfig({
  entry: {
    index: 'src/index.ts',
    'transport/webrtc': 'src/transport/webrtc.ts',
    'transport/node-udp': 'src/transport/node-udp.ts',
    'transport/protobuf-es': 'src/transport/protobuf-es.ts',
    'transport/connect': 'src/transport/connect.ts',
  },
  dts: true,
})
