import UIKit
import DCloudUniappRuntime

// 继承 SDK 的场景代理，其余场景回调（前后台、openURL……）都由它处理。
class SceneDelegate: UniAppRootSceneDelegate {
    override func scene(_ scene: UIScene,
                        willConnectTo session: UISceneSession,
                        options connectionOptions: UIScene.ConnectionOptions) {
        super.scene(scene, willConnectTo: session, options: connectionOptions)
        guard let windowScene = scene as? UIWindowScene else { return }

        // super 会按名字加载 Main.storyboard（见那个文件里的注释），根控制器此时是里面那个
        // 空页面 —— 所以这里无条件换成 uni-app 的根控制器，而不是「为空才设」。
        let window = self.window ?? UIWindow(windowScene: windowScene)
        // uni.exit() 会销毁 app 实例，所以先确认它在（照 Demo 的 pushUniViewController）。
        if UniSDKEngine.shared.getAppManager()?.getCurrentApp() == nil {
            UniSDKEngine.shared.getAppManager()?.create()
        }
        window.rootViewController = UniAppRootViewController()
        self.window = window
        (UIApplication.shared.delegate as? AppDelegate)?.window = window
        window.makeKeyAndVisible()
    }
}
