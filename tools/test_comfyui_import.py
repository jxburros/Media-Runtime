import unittest
from comfyui_import import translate


def graph():
    return {
        '1': {'class_type':'CheckpointLoaderSimple','inputs':{'ckpt_name':'sdxl.safetensors'}},
        '2': {'class_type':'CLIPTextEncode','inputs':{'clip':['1',1],'text':'portrait'}},
        '3': {'class_type':'CLIPTextEncode','inputs':{'clip':['1',1],'text':'blur'}},
        '4': {'class_type':'EmptyLatentImage','inputs':{'width':1024,'height':1024,'batch_size':1}},
        '5': {'class_type':'KSampler','inputs':{'model':['1',0],'positive':['2',0],'negative':['3',0],'latent_image':['4',0],'seed':7,'steps':30,'cfg':6,'sampler_name':'euler','scheduler':'normal','denoise':1}},
        '6': {'class_type':'VAEDecode','inputs':{'samples':['5',0],'vae':['1',2]}},
        '7': {'class_type':'SaveImage','inputs':{'images':['6',0],'filename_prefix':'portrait'}},
    }


class ImportTests(unittest.TestCase):
    def test_exact_settings_and_model_mapping(self):
        result=translate(graph(),{'sdxl.safetensors':'registered-sdxl'},'imported')
        self.assertEqual(result['request']['model_id'],'registered-sdxl')
        self.assertEqual(result['request']['inputs'],{'prompt':'portrait','negative_prompt':'blur'})
        self.assertEqual(result['recipe']['operations'][0]['defaults']['seed'],7)
        self.assertEqual(result['recipe']['operations'][0]['defaults']['scheduler'],'euler')
        self.assertEqual(result['recipe']['publish'],['generate/image'])

    def test_custom_nodes_unmapped_models_and_rewired_inputs_refused(self):
        with self.assertRaisesRegex(ValueError,'explicit mapping'):translate(graph(),{},'test')
        custom=graph();custom['8']={'class_type':'ExecutePython','inputs':{'code':'raise RuntimeError()'}}
        with self.assertRaisesRegex(ValueError,'unsupported class'):translate(custom,{},'test')
        rewired=graph();rewired['6']['inputs']['vae']=['1',1]
        with self.assertRaisesRegex(ValueError,'Expected link'):translate(rewired,{},'test')

    def test_no_silent_seed_size_sampler_or_denoise_changes(self):
        for field,value in [('seed',2**40),('seed',True),('denoise',0.5),('sampler_name','lms')]:
            source=graph();source['5']['inputs'][field]=value
            with self.subTest(field=field),self.assertRaises(ValueError):translate(source,{'sdxl.safetensors':'sdxl'},'test')
        source=graph();source['4']['inputs']['width']=1025
        with self.assertRaises(ValueError):translate(source,{'sdxl.safetensors':'sdxl'},'test')

    def test_batch_uses_base_publish_id_expanded_by_runtime(self):
        source=graph();source['4']['inputs']['batch_size']=4
        result=translate(source,{'sdxl.safetensors':'sdxl'},'test')
        self.assertEqual(result['recipe']['operations'][0]['defaults']['count'],4)
        self.assertEqual(result['recipe']['publish'],['generate/image'])

    def test_non_euler_pair_translates_explicitly(self):
        source=graph();source['5']['inputs'].update(sampler_name='dpmpp_2m',scheduler='karras')
        result=translate(source,{'sdxl.safetensors':'sdxl'},'test')
        self.assertEqual(result['recipe']['operations'][0]['defaults']['sampler'],'dpmpp-2m')


if __name__=='__main__':unittest.main()
